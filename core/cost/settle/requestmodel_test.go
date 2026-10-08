package settle

import (
	"context"
	"math"
	"testing"

	"github.com/rossoctl/cortex/core/cost/pricing"
	"github.com/rossoctl/cortex/core/pipeline"
)

// remodel changes every request's model to model with SetRequestModel, as a router
// does, and keeps the error for the test.
type remodel struct {
	model string
	err   *error
}

func (remodel) Name() string { return "remodel" }
func (remodel) Capabilities() pipeline.PluginCapabilities {
	return pipeline.PluginCapabilities{WritesRequestBody: true}
}
func (r remodel) OnRequest(_ context.Context, pctx *pipeline.Context) pipeline.Action {
	*r.err = pctx.SetRequestModel(r.model)
	return pipeline.Action{Type: pipeline.Continue}
}
func (remodel) OnResponse(context.Context, *pipeline.Context) pipeline.Action {
	return pipeline.Action{Type: pipeline.Continue}
}

// Settlement prices the model the request was sent for. The table knows only the
// server's model, so the request is modelled at all only because SetRequestModel
// moved Model to it.
func TestSettle_PricesTheModelSetRequestModelChose(t *testing.T) {
	var r pricing.Rates
	for tier, perToken := range tierMicros {
		r.Base[tier], r.Set[tier] = perToken*1e-6, true
	}
	tab, err := pricing.NewTable([]pricing.Entry{{Host: "*", Model: "glm-5.3", Rates: r, Prov: pricing.ProvConfigured}})
	if err != nil {
		t.Fatal(err)
	}
	glmOnly := pricing.NewRegistry(tab)

	pctx := ctx(nil, 1000, 500)
	pctx.Direction = pipeline.Outbound
	pctx.Body = []byte(`{"model":"claude-opus-5","max_tokens":1024}`)
	if Settle(pctx, glmOnly).HasModelled {
		t.Fatal("claude-opus-5 is priced before the change, so this test proves nothing")
	}

	var setErr error
	p, err := pipeline.New([]pipeline.Plugin{remodel{model: "glm-5.3", err: &setErr}})
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}
	p.Run(context.Background(), pctx)
	if setErr != nil {
		t.Fatalf("SetRequestModel: %v", setErr)
	}

	got := Settle(pctx, glmOnly)
	if !got.HasModelled || math.Abs(got.ModelledUSD-fallbackUSD(1000, 500)) > 1e-12 {
		t.Errorf("modelled = %v %v, want %v at glm-5.3's rates", got.HasModelled, got.ModelledUSD, fallbackUSD(1000, 500))
	}
}
