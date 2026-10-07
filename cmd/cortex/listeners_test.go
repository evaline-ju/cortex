package main

import (
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/rossoctl/cortex/core/config"
)

func planNames(plan []plannedListener) []string {
	names := make([]string, len(plan))
	for i, p := range plan {
		names[i] = p.name
	}
	return names
}

// The plan must name exactly the sockets main serves, under the same conditions: a socket
// reserved and never served accepts connections nothing answers, and one served without a
// reservation is bound after the data it was meant to guard. take and claimed catch either
// at boot; this pins each deployment shape's list so neither boot is the first to find out.
func TestListenerPlan_NamesWhatEachShapeServes(t *testing.T) {
	off := false
	listener := config.ListenerConfig{
		ReverseProxyAddr:       "127.0.0.1:1",
		TransparentInboundAddr: "127.0.0.1:2",
		ForwardProxyAddr:       "127.0.0.1:3",
		TransparentProxyAddr:   "127.0.0.1:4",
		SessionAPIAddr:         "127.0.0.1:5",
		HealthAddr:             "127.0.0.1:6",
	}
	cfgWith := func(edit func(*config.Config)) *config.Config {
		c := &config.Config{Listener: listener, Stats: config.StatsConfig{StatsAddress: "127.0.0.1:7"}}
		edit(c)
		return c
	}
	cases := []struct {
		name  string
		cfg   *config.Config
		local bool
		want  []string
	}{
		{
			name:  "laptop: forward only, no iptables to feed a transparent listener",
			cfg:   cfgWith(func(c *config.Config) { c.Listener.Roles = []string{config.RoleForward} }),
			local: true,
			want:  []string{"forward-proxy", "stats", "session-api", "health"},
		},
		{
			name: "pod: both roles, reverse-proxy inbound, enforce-redirect egress",
			cfg:  cfgWith(func(*config.Config) {}),
			want: []string{"reverse-proxy", "forward-proxy", "transparent-proxy", "stats", "session-api", "health"},
		},
		{
			name: "pod: transparent inbound",
			cfg: cfgWith(func(c *config.Config) {
				c.Listener.InboundInterception = config.InboundInterceptionTransparent
			}),
			want: []string{"transparent-inbound", "forward-proxy", "transparent-proxy", "stats", "session-api", "health"},
		},
		{
			name: "local install never starts transparent inbound either",
			cfg: cfgWith(func(c *config.Config) {
				c.Listener.InboundInterception = config.InboundInterceptionTransparent
			}),
			local: true,
			want:  []string{"forward-proxy", "stats", "session-api", "health"},
		},
		{
			name: "no transparent_proxy_addr means no transparent proxy",
			cfg:  cfgWith(func(c *config.Config) { c.Listener.TransparentProxyAddr = "" }),
			want: []string{"reverse-proxy", "forward-proxy", "stats", "session-api", "health"},
		},
		{
			name: "sessions off means no session API",
			cfg:  cfgWith(func(c *config.Config) { c.Session.Enabled = &off }),
			want: []string{"reverse-proxy", "forward-proxy", "transparent-proxy", "stats", "health"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := listenerPlan(tc.cfg, tc.local)
			if err != nil {
				t.Fatal(err)
			}
			if got := planNames(plan); !slices.Equal(got, tc.want) {
				t.Errorf("plan = %v, want %v", got, tc.want)
			}
		})
	}
}

// An empty transparent_inbound_addr was an error from the bind; reserving must keep it one
// rather than binding "" — which net.Listen takes as a random port on every interface.
func TestListenerPlan_EmptyTransparentInboundIsAnError(t *testing.T) {
	cfg := &config.Config{Listener: config.ListenerConfig{
		InboundInterception: config.InboundInterceptionTransparent,
	}}
	if _, err := listenerPlan(cfg, false); err == nil || !strings.Contains(err.Error(), "transparent_inbound_addr is empty") {
		t.Fatalf("err = %v, want the empty-address error", err)
	}
}

// All or nothing: a port that is taken fails the reservation, names the socket, and leaves
// none of the others held — the process is about to exit, and a later attempt must find
// them free.
func TestReserveListeners_AllOrNothing(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	free := freeAddr(t)

	_, err = reserveListeners([]plannedListener{{"stats", free}, {"health", held.Addr().String()}})
	if err == nil || !strings.Contains(err.Error(), "health") || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("err = %v, want the health socket named as in use", err)
	}
	again, err := net.Listen("tcp", free)
	if err != nil {
		t.Fatalf("the socket reserved before the failure is still held: %v", err)
	}
	_ = again.Close()
}

func TestReservedListeners_TakeAndClaimed(t *testing.T) {
	r, err := reserveListeners([]plannedListener{{"stats", "127.0.0.1:0"}, {"health", "127.0.0.1:0"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.closeAll()
	if ln := r.take("stats"); ln == nil {
		t.Fatal("take returned nil for a reserved socket")
	}
	if err := r.claimed(); err == nil || !strings.Contains(err.Error(), "health") {
		t.Errorf("claimed() = %v, want health named as reserved but never served", err)
	}
	r.take("health")
	if err := r.claimed(); err != nil {
		t.Errorf("claimed() = %v after every socket was taken", err)
	}
}
