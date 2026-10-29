package management

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cost"
)

// GetCostPolicies lists all active price policies.
func (h *Handler) GetCostPolicies(c *gin.Context) {
	st := h.GetPolicyStore()
	if st == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "policy store unavailable"})
		return
	}
	policies := st.ListActivePolicies()
	c.JSON(http.StatusOK, gin.H{
		"policies": policies,
		"count":    len(policies),
	})
}

// GetCostPolicyByModel retrieves an active policy for the specified provider and model.
func (h *Handler) GetCostPolicyByModel(c *gin.Context) {
	st := h.GetPolicyStore()
	if st == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "policy store unavailable"})
		return
	}
	provider := c.Param("provider")
	model := c.Param("model")
	policy, ok := st.GetActivePolicy(provider, model)
	if !ok || policy == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "policy not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"policy": policy,
	})
}

// PostCostPoliciesDryRun evaluates a proposed policy without modifying state.
func (h *Handler) PostCostPoliciesDryRun(c *gin.Context) {
	st := h.GetPolicyStore()
	if st == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "policy store unavailable"})
		return
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}

	snap, err := cost.UnmarshalPolicySnapshotStrict(body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	report, err := st.DryRun(snap)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"allowed": report.Allowed,
		"report":  report,
	})
}

// PostCostPoliciesActivate validates and activates a policy snapshot.
func (h *Handler) PostCostPoliciesActivate(c *gin.Context) {
	st := h.GetPolicyStore()
	if st == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "policy store unavailable"})
		return
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}

	snap, err := cost.UnmarshalPolicySnapshotStrict(body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	force := strings.EqualFold(c.Query("force"), "true")
	activated, err := st.Activate(snap, force)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":   true,
		"activated": activated,
	})
}

type rollbackPolicyRequest struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	TargetVersion string `json:"target_version,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// PostCostPoliciesRollback rolls back an active policy to a previously approved version.
func (h *Handler) PostCostPoliciesRollback(c *gin.Context) {
	st := h.GetPolicyStore()
	if st == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "policy store unavailable"})
		return
	}

	var req rollbackPolicyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if req.Provider == "" || req.Model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider and model are required"})
		return
	}

	rolledBack, err := st.Rollback(req.Provider, req.Model, req.TargetVersion)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":     true,
		"rolled_back": rolledBack,
	})
}

type revokePolicyRequest struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Version  string `json:"version"`
	Reason   string `json:"reason,omitempty"`
}

// PostCostPoliciesRevoke revokes a policy version so it can never be used or re-imported.
func (h *Handler) PostCostPoliciesRevoke(c *gin.Context) {
	st := h.GetPolicyStore()
	if st == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "policy store unavailable"})
		return
	}

	var req revokePolicyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if req.Provider == "" || req.Model == "" || req.Version == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider, model, and version are required"})
		return
	}

	if err := st.Revoke(req.Provider, req.Model, req.Version, req.Reason); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
	})
}

// GetCostPoliciesAudit returns recent cost policy audit events.
func (h *Handler) GetCostPoliciesAudit(c *gin.Context) {
	st := h.GetPolicyStore()
	if st == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "policy store unavailable"})
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	events := st.ListAuditEvents(limit, offset)

	c.JSON(http.StatusOK, gin.H{
		"events": events,
		"count":  len(events),
	})
}

// Compile check for unused error
var _ = errors.New
