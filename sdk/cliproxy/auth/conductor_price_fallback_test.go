package auth

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cost"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type mockFallbackTestExecutor struct {
	id           string
	calls        *int64
	failStatuses map[string]int // model -> HTTP status
}

func (e *mockFallbackTestExecutor) Identifier() string { return e.id }

func (e *mockFallbackTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *mockFallbackTestExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *mockFallbackTestExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	atomic.AddInt64(e.calls, 1)
	if status, shouldFail := e.failStatuses[req.Model]; shouldFail {
		return cliproxyexecutor.Response{}, &Error{
			HTTPStatus: status,
			Message:    "mock error",
		}
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"result":"ok"}`)}, nil
}

func (e *mockFallbackTestExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *mockFallbackTestExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	atomic.AddInt64(e.calls, 1)
	if status, shouldFail := e.failStatuses[req.Model]; shouldFail {
		return cliproxyexecutor.Response{}, &Error{
			HTTPStatus: status,
			Message:    "mock error",
		}
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"total_tokens":10}`)}, nil
}

func setupFallbackManagerTest(t *testing.T, cfg *config.Config) (*Manager, *int64, map[string]int) {
	t.Helper()
	mgr := NewManager(nil, nil, nil)
	mgr.SetConfig(cfg)
	var calls int64
	failStatuses := make(map[string]int)

	for _, p := range cfg.OpenAICompatibility {
		pKey := util.OpenAICompatibleProviderKey(p.Name)
		exec := &mockFallbackTestExecutor{
			id:           pKey,
			calls:        &calls,
			failStatuses: failStatuses,
		}
		mgr.RegisterExecutor(exec)
		for idx, keyEntry := range p.APIKeyEntries {
			authID := p.Name + "-auth-" + string(rune('0'+idx))
			auth := &Auth{
				ID:       authID,
				Provider: pKey,
				Status:   StatusActive,
				Attributes: map[string]string{
					"api_key":      keyEntry.APIKey,
					"base_url":     p.BaseURL,
					"compat_name":  p.Name,
					"provider_key": pKey,
				},
			}
			if _, err := mgr.Register(context.Background(), auth); err != nil {
				t.Fatalf("register auth: %v", err)
			}

			var regModels []*registry.ModelInfo
			for _, m := range p.Models {
				publicModel := m.Alias
				if publicModel == "" {
					publicModel = m.Name
				}
				regModels = append(regModels, &registry.ModelInfo{ID: publicModel})
				if m.Name != "" && m.Name != publicModel {
					regModels = append(regModels, &registry.ModelInfo{ID: m.Name})
				}
			}
			registry.GetGlobalRegistry().RegisterClient(authID, pKey, regModels)
			t.Cleanup(func() {
				registry.GetGlobalRegistry().UnregisterClient(authID)
			})
			mgr.ReconcileRegistryModelStates(context.Background(), authID)
			mgr.RefreshSchedulerEntry(authID)
		}
	}
	mgr.RefreshAPIKeyModelAlias()

	return mgr, &calls, failStatuses
}

func TestFreeProvider429CannotFallbackToPaid(t *testing.T) {
	// Item 15: 免费 Provider 429 后不能调用收费 Provider
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:          "prov-free",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "free-key"}},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "chat",
						Alias: "chat",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus: config.PriceStatusFreeConfirmed,
							BillingUnit: config.BillingUnitTokens,
						},
					},
				},
			},
			{
				Name:          "prov-paid",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "paid-key"}},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "chat",
						Alias: "chat",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          config.PriceStatusPaidConfirmed,
							BillingUnit:          config.BillingUnitTokens,
							PromptPer1M:          2.0,
							PromptConfigured:     true,
							CompletionPer1M:      4.0,
							CompletionConfigured: true,
						},
					},
				},
			},
		},
	}

	mgr, calls, failStatuses := setupFallbackManagerTest(t, cfg)
	failStatuses["chat"] = 429

	_, primaryPol, _, _ := cost.ResolveModelPolicy(cfg, "prov-free", "chat", "chat")
	guard := cost.NewFallbackGuard(
		cfg,
		"prov-free",
		"chat",
		"chat",
		cost.CostEstimate{PriceStatus: config.PriceStatusFreeConfirmed, ExplicitlyFree: true, Known: true},
		primaryPol,
		[]string{"prov-free"}, // prov-paid was not allowed for free primary!
		cost.FromTokens(100, 100, 0, 0, 0),
		"req-1",
	)

	ctx := cost.WithFallbackGuard(context.Background(), guard)
	providers := []string{util.OpenAICompatibleProviderKey("prov-free"), util.OpenAICompatibleProviderKey("prov-paid")}
	_, err := mgr.Execute(ctx, providers, cliproxyexecutor.Request{Model: "chat"}, cliproxyexecutor.Options{})

	// Should fail without calling prov-paid!
	if err == nil {
		t.Fatal("expected error, got success")
	}
	if *calls != 1 {
		t.Fatalf("expected 1 call to prov-free, got %d calls (prov-paid was called!)", *calls)
	}
}

func TestFreeProvider500CannotFallbackToPaid(t *testing.T) {
	// Item 16: 免费 Provider 500 后不能调用收费 Provider
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:          "prov-free",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "free-key"}},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "chat",
						Alias: "chat",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus: config.PriceStatusFreeConfirmed,
							BillingUnit: config.BillingUnitTokens,
						},
					},
				},
			},
			{
				Name:          "prov-paid",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "paid-key"}},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "chat",
						Alias: "chat",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          config.PriceStatusPaidConfirmed,
							BillingUnit:          config.BillingUnitTokens,
							PromptPer1M:          2.0,
							PromptConfigured:     true,
							CompletionPer1M:      4.0,
							CompletionConfigured: true,
						},
					},
				},
			},
		},
	}

	mgr, calls, failStatuses := setupFallbackManagerTest(t, cfg)
	failStatuses["chat"] = 500

	_, primaryPol, _, _ := cost.ResolveModelPolicy(cfg, "prov-free", "chat", "chat")
	guard := cost.NewFallbackGuard(
		cfg,
		"prov-free",
		"chat",
		"chat",
		cost.CostEstimate{PriceStatus: config.PriceStatusFreeConfirmed, ExplicitlyFree: true, Known: true},
		primaryPol,
		[]string{"prov-free"},
		cost.FromTokens(100, 100, 0, 0, 0),
		"req-1",
	)

	ctx := cost.WithFallbackGuard(context.Background(), guard)
	providers := []string{util.OpenAICompatibleProviderKey("prov-free"), util.OpenAICompatibleProviderKey("prov-paid")}
	_, err := mgr.Execute(ctx, providers, cliproxyexecutor.Request{Model: "chat"}, cliproxyexecutor.Options{})

	if err == nil {
		t.Fatal("expected error, got success")
	}
	if *calls != 1 {
		t.Fatalf("expected 1 call to prov-free, got %d calls", *calls)
	}
}

func TestPaidFailureCanFallbackToSameOrLowerPrice(t *testing.T) {
	// Item 17 & 18: paid 失败可以调用同价或低价 Provider
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:          "prov-paid-primary",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "k1"}},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "chat-primary",
						Alias: "chat",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          config.PriceStatusPaidConfirmed,
							BillingUnit:          config.BillingUnitTokens,
							PromptPer1M:          2.0,
							PromptConfigured:     true,
							CompletionPer1M:      4.0,
							CompletionConfigured: true,
						},
					},
				},
			},
			{
				Name:          "prov-paid-lower",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "k2"}},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "chat-lower",
						Alias: "chat",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          config.PriceStatusPaidConfirmed,
							BillingUnit:          config.BillingUnitTokens,
							PromptPer1M:          1.0, // cheaper!
							PromptConfigured:     true,
							CompletionPer1M:      2.0,
							CompletionConfigured: true,
						},
					},
				},
			},
		},
	}

	mgr, calls, failStatuses := setupFallbackManagerTest(t, cfg)
	failStatuses["chat-primary"] = 503

	_, primaryPol, _, _ := cost.ResolveModelPolicy(cfg, "prov-paid-primary", "chat", "chat")
	guard := cost.NewFallbackGuard(
		cfg,
		"prov-paid-primary",
		"chat",
		"chat",
		cost.CostEstimate{
			PriceStatus: config.PriceStatusPaidConfirmed,
			BillingUnit: config.BillingUnitTokens,
			AmountKnown: true,
			AmountNanos: 2_000_000_000,
		},
		primaryPol,
		[]string{"prov-paid-primary", "prov-paid-lower"},
		cost.FromTokens(500_000, 250_000, 0, 0, 0),
		"req-fallback-paid",
	)

	ctx := cost.WithFallbackGuard(context.Background(), guard)
	providers := []string{util.OpenAICompatibleProviderKey("prov-paid-primary"), util.OpenAICompatibleProviderKey("prov-paid-lower")}
	resp, err := mgr.Execute(ctx, providers, cliproxyexecutor.Request{Model: "chat"}, cliproxyexecutor.Options{})

	if err != nil {
		t.Fatalf("expected successful fallback to cheaper provider, got err: %v", err)
	}
	if len(resp.Payload) == 0 {
		t.Fatal("expected non-empty response payload")
	}
	if *calls != 2 {
		t.Fatalf("expected 2 calls, got %d", *calls)
	}
}

func TestPaidFailureCannotFallbackToHigherPrice(t *testing.T) {
	// Item 19: paid 失败不能调用高价 Provider
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:          "prov-paid-cheap",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "k1"}},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "chat-cheap",
						Alias: "chat",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          config.PriceStatusPaidConfirmed,
							BillingUnit:          config.BillingUnitTokens,
							PromptPer1M:          1.0,
							PromptConfigured:     true,
							CompletionPer1M:      2.0,
							CompletionConfigured: true,
						},
					},
				},
			},
			{
				Name:          "prov-paid-expensive",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "k2"}},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "chat-expensive",
						Alias: "chat",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          config.PriceStatusPaidConfirmed,
							BillingUnit:          config.BillingUnitTokens,
							PromptPer1M:          10.0, // more expensive!
							PromptConfigured:     true,
							CompletionPer1M:      20.0,
							CompletionConfigured: true,
						},
					},
				},
			},
		},
	}

	mgr, calls, failStatuses := setupFallbackManagerTest(t, cfg)
	failStatuses["chat-cheap"] = 503

	_, primaryPol, _, _ := cost.ResolveModelPolicy(cfg, "prov-paid-cheap", "chat", "chat")
	guard := cost.NewFallbackGuard(
		cfg,
		"prov-paid-cheap",
		"chat",
		"chat",
		cost.CostEstimate{
			PriceStatus: config.PriceStatusPaidConfirmed,
			BillingUnit: config.BillingUnitTokens,
			AmountKnown: true,
			AmountNanos: 1_000_000_000,
		},
		primaryPol,
		[]string{"prov-paid-cheap"}, // expensive provider filtered out!
		cost.FromTokens(500_000, 250_000, 0, 0, 0),
		"req-high-fallback",
	)

	ctx := cost.WithFallbackGuard(context.Background(), guard)
	providers := []string{util.OpenAICompatibleProviderKey("prov-paid-cheap"), util.OpenAICompatibleProviderKey("prov-paid-expensive")}
	_, err := mgr.Execute(ctx, providers, cliproxyexecutor.Request{Model: "chat"}, cliproxyexecutor.Options{})

	if err == nil {
		t.Fatal("expected error, got success")
	}
	if *calls != 1 {
		t.Fatalf("expected only 1 call to cheap provider, got %d calls", *calls)
	}
}

func TestAliasPoolFreeCannotFallbackToPaidUpstream(t *testing.T) {
	// Item 21: alias 池 free 失败不能调用 paid
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:          "pool-prov",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "k-pool"}},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "upstream-free",
						Alias: "my-pool",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus: config.PriceStatusFreeConfirmed,
							BillingUnit: config.BillingUnitTokens,
						},
					},
					{
						Name:  "upstream-paid",
						Alias: "my-pool",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          config.PriceStatusPaidConfirmed,
							BillingUnit:          config.BillingUnitTokens,
							PromptPer1M:          2.0,
							PromptConfigured:     true,
							CompletionPer1M:      4.0,
							CompletionConfigured: true,
						},
					},
				},
			},
		},
	}

	mgr, calls, failStatuses := setupFallbackManagerTest(t, cfg)
	failStatuses["upstream-free"] = 500

	_, primaryPol, _, _ := cost.ResolveModelPolicy(cfg, "pool-prov", "upstream-free", "chat")
	guard := cost.NewFallbackGuard(
		cfg,
		"pool-prov",
		"my-pool",
		"chat",
		cost.CostEstimate{PriceStatus: config.PriceStatusFreeConfirmed, ExplicitlyFree: true, Known: true},
		primaryPol,
		[]string{"pool-prov"},
		cost.FromTokens(100, 100, 0, 0, 0),
		"req-pool-1",
	)

	ctx := cost.WithFallbackGuard(context.Background(), guard)
	providers := []string{util.OpenAICompatibleProviderKey("pool-prov")}
	_, err := mgr.Execute(ctx, providers, cliproxyexecutor.Request{Model: "my-pool"}, cliproxyexecutor.Options{})

	if err == nil {
		t.Fatal("expected error when upstream-free fails and paid is blocked")
	}
	if *calls != 1 {
		t.Fatalf("expected 1 call (upstream-free only), got %d calls (upstream-paid was called!)", *calls)
	}
}

func TestExecuteCountProtectedByFallbackGuard(t *testing.T) {
	// Item 24: ExecuteCount 同样受保护
	cfg := &config.Config{
		CostGuard: config.CostGuardConfig{
			Enabled: true,
		},
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:          "prov-free",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "k1"}},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "count-model",
						Alias: "count-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus: config.PriceStatusFreeConfirmed,
							BillingUnit: config.BillingUnitTokens,
						},
					},
				},
			},
			{
				Name:          "prov-paid",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "k2"}},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "count-model",
						Alias: "count-model",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:          config.PriceStatusPaidConfirmed,
							BillingUnit:          config.BillingUnitTokens,
							PromptPer1M:          2.0,
							PromptConfigured:     true,
							CompletionPer1M:      4.0,
							CompletionConfigured: true,
						},
					},
				},
			},
		},
	}

	mgr, calls, failStatuses := setupFallbackManagerTest(t, cfg)
	failStatuses["count-model"] = 429

	_, primaryPol, _, _ := cost.ResolveModelPolicy(cfg, "prov-free", "count-model", "chat")
	guard := cost.NewFallbackGuard(
		cfg,
		"prov-free",
		"count-model",
		"chat",
		cost.CostEstimate{PriceStatus: config.PriceStatusFreeConfirmed, ExplicitlyFree: true, Known: true},
		primaryPol,
		[]string{"prov-free"},
		cost.FromTokens(100, 100, 0, 0, 0),
		"req-count-1",
	)

	ctx := cost.WithFallbackGuard(context.Background(), guard)
	providers := []string{util.OpenAICompatibleProviderKey("prov-free"), util.OpenAICompatibleProviderKey("prov-paid")}
	_, err := mgr.ExecuteCount(ctx, providers, cliproxyexecutor.Request{Model: "count-model"}, cliproxyexecutor.Options{})

	if err == nil {
		t.Fatal("expected error on count tokens fallback to paid")
	}
	if *calls != 1 {
		t.Fatalf("expected 1 call (prov-free only), got %d calls", *calls)
	}
}
