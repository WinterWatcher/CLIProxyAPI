package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestGetOpenAICompatIncludesDisableCooling(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	requestRetry := 0
	disableCooling := true
	h := NewHandlerWithoutConfigFilePath(&config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "Mimo CN",
				BaseURL: "https://token-plan-cn.xiaomimimo.com/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "test-key"},
				},
				Models: []config.OpenAICompatibilityModel{
					{Name: "mimo-v2.5", Alias: ""},
				},
				SupportPromptCacheKey: true,
				DisableCooling:        &disableCooling,
				RequestRetry:          &requestRetry,
			},
		},
	}, nil)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/openai-compatibility", nil)
	h.GetOpenAICompat(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var body struct {
		OpenAICompatibility []struct {
			SupportPromptCacheKey *bool `json:"support-prompt-cache-key"`
			DisableCooling        *bool `json:"disable-cooling"`
			RequestRetry          *int  `json:"request-retry"`
		} `json:"openai-compatibility"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(body.OpenAICompatibility) != 1 {
		t.Fatalf("expected 1 openai-compatibility entry, got %d", len(body.OpenAICompatibility))
	}
	if body.OpenAICompatibility[0].SupportPromptCacheKey == nil || !*body.OpenAICompatibility[0].SupportPromptCacheKey {
		t.Fatalf("expected support-prompt-cache-key to be present and true, got %#v", body.OpenAICompatibility[0].SupportPromptCacheKey)
	}
	if body.OpenAICompatibility[0].DisableCooling == nil || !*body.OpenAICompatibility[0].DisableCooling {
		t.Fatalf("expected disable-cooling to be present and true, got %#v", body.OpenAICompatibility[0].DisableCooling)
	}
	if body.OpenAICompatibility[0].RequestRetry == nil || *body.OpenAICompatibility[0].RequestRetry != 0 {
		t.Fatalf("expected request-retry to be present and 0, got %#v", body.OpenAICompatibility[0].RequestRetry)
	}
}

func TestGetOpenAICompatIncludesCostPolicy(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	h := NewHandlerWithoutConfigFilePath(&config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{
			{
				Name:    "Mimo CN",
				BaseURL: "https://token-plan-cn.xiaomimimo.com/v1",
				CostPolicy: &config.ProviderCostPolicy{
					Mode:               "locked",
					UnknownPriceAction: config.CostActionDeny,
					ProviderBudget: config.ProviderBudgetPolicy{
						DailyUSD:   5.0,
						MonthlyUSD: 50.0,
					},
				},
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{
					{APIKey: "test-secret-key"},
				},
				Models: []config.OpenAICompatibilityModel{
					{
						Name:  "mimo-v2.5",
						Alias: "mimo-v2.5",
						CostPolicy: &config.ModelCostPolicy{
							PriceStatus:     config.PriceStatusPaidConfirmed,
							BillingUnit:     config.BillingUnitTokens,
							PromptPer1M:     0.5,
							CompletionPer1M: 1.5,
						},
					},
				},
			},
		},
	}, nil)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/openai-compatibility", nil)
	h.GetOpenAICompat(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var body struct {
		OpenAICompatibility []struct {
			Name       string                     `json:"name"`
			CostPolicy *config.ProviderCostPolicy `json:"cost-policy"`
			Models     []struct {
				Name       string                  `json:"name"`
				CostPolicy *config.ModelCostPolicy `json:"cost-policy"`
			} `json:"models"`
		} `json:"openai-compatibility"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(body.OpenAICompatibility) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(body.OpenAICompatibility))
	}
	entry := body.OpenAICompatibility[0]
	if entry.CostPolicy == nil || entry.CostPolicy.Mode != "locked" {
		t.Fatalf("expected provider cost-policy with mode locked, got %#v", entry.CostPolicy)
	}
	if entry.CostPolicy.ProviderBudget.DailyUSD != 5.0 {
		t.Fatalf("expected daily-usd 5.0, got %v", entry.CostPolicy.ProviderBudget.DailyUSD)
	}
	if len(entry.Models) != 1 || entry.Models[0].CostPolicy == nil {
		t.Fatalf("expected model cost policy, got %#v", entry.Models)
	}
	if entry.Models[0].CostPolicy.PriceStatus != config.PriceStatusPaidConfirmed {
		t.Fatalf("expected price status paid_confirmed, got %s", entry.Models[0].CostPolicy.PriceStatus)
	}
	if entry.Models[0].CostPolicy.PromptPer1M != 0.5 {
		t.Fatalf("expected prompt price 0.5, got %v", entry.Models[0].CostPolicy.PromptPer1M)
	}
}
