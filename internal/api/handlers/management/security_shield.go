package management

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const (
	primaryHealthPath = "/var/lib/skyvpn/service-health.json"
	primaryRulesPath  = "/usr/local/etc/cloudcode-rules.json"
	fallbackRulesPath = "scripts/cloudcode-rules.json"
)

// SecurityShieldStatusResponse represents comprehensive security defense telemetry
type SecurityShieldStatusResponse struct {
	Healthy           bool                   `json:"healthy"`
	GatewayRunning    bool                   `json:"gateway_running"`
	WafRiskLevel      string                 `json:"waf_risk_level"` // "safe", "warning", "critical"
	CanaryStatus      int                    `json:"canary_status"`
	CanaryHealthy     bool                   `json:"canary_healthy"`
	CanaryReason      string                 `json:"canary_reason"`
	LastCanaryTime    string                 `json:"last_canary_time"`
	CanaryLatencySec  float64                `json:"canary_latency_sec"`
	AccountProtection map[string]interface{} `json:"account_protection"`
	RuleStats         map[string]int         `json:"rule_stats"`
	ActiveVersion     string                 `json:"active_version"`
}

func getRulesPath() string {
	if _, err := os.Stat(primaryRulesPath); err == nil {
		return primaryRulesPath
	}
	if _, err := os.Stat(fallbackRulesPath); err == nil {
		return fallbackRulesPath
	}
	return primaryRulesPath
}

// GetSecurityShieldStatus handles GET /v0/management/security-shield/status
func (h *Handler) GetSecurityShieldStatus(c *gin.Context) {
	resp := SecurityShieldStatusResponse{
		Healthy:        true,
		GatewayRunning: true,
		WafRiskLevel:   "safe",
		CanaryStatus:   200,
		CanaryHealthy:  true,
		CanaryReason:   "canary_ok",
		LastCanaryTime: time.Now().Format(time.RFC3339),
		RuleStats:      map[string]int{"strip_rules": 0, "custom_rules": 0, "audit_terms": 0},
		AccountProtection: map[string]interface{}{
			"email":               "bubbeisser112@gmail.com",
			"status":              "safe",
			"policy":              "single_account_locked_zero_retry",
			"sanitization_engine": "antigravity_zero_leak_v3",
		},
	}

	// 1. Probe local gateway port 127.0.0.1:28100
	conn, err := net.DialTimeout("tcp", "127.0.0.1:28100", 300*time.Millisecond)
	if err != nil {
		resp.GatewayRunning = false
	} else {
		_ = conn.Close()
		resp.GatewayRunning = true
	}

	// 2. Read dual-track canary probe state
	if healthBytes, err := os.ReadFile(primaryHealthPath); err == nil {
		var healthMap map[string]interface{}
		if err := json.Unmarshal(healthBytes, &healthMap); err == nil {
			if cs, ok := healthMap["canary_status"].(float64); ok {
				resp.CanaryStatus = int(cs)
			}
			if ch, ok := healthMap["canary_healthy"].(bool); ok {
				resp.CanaryHealthy = ch
			}
			if cr, ok := healthMap["canary_reason"].(string); ok {
				resp.CanaryReason = cr
			}
			if sec, ok := healthMap["seconds"].(float64); ok {
				resp.CanaryLatencySec = sec
			}
			if t, ok := healthMap["time"].(string); ok {
				resp.LastCanaryTime = t
			}
		}
	}

	// Determine risk level based on canary telemetry
	if resp.CanaryStatus == 429 {
		resp.WafRiskLevel = "warning"
		resp.Healthy = false
	} else if resp.CanaryStatus == 403 || resp.CanaryStatus == 401 {
		resp.WafRiskLevel = "critical"
		resp.Healthy = false
	} else if !resp.GatewayRunning {
		resp.WafRiskLevel = "warning"
		resp.Healthy = false
	} else {
		resp.WafRiskLevel = "safe"
		resp.Healthy = true
	}

	// 3. Read rules stats from cloudcode-rules.json
	rulesPath := getRulesPath()
	if rulesBytes, err := os.ReadFile(rulesPath); err == nil {
		var rulesMap map[string]interface{}
		if err := json.Unmarshal(rulesBytes, &rulesMap); err == nil {
			if v, ok := rulesMap["version"].(string); ok {
				resp.ActiveVersion = v
			}
			if strips, ok := rulesMap["harness_strip_sections"].([]interface{}); ok {
				resp.RuleStats["strip_rules"] = len(strips)
			}
			if custom, ok := rulesMap["custom_replacements"].([]interface{}); ok {
				resp.RuleStats["custom_rules"] = len(custom)
			}
			if terms, ok := rulesMap["leak_audit_terms"].([]interface{}); ok {
				resp.RuleStats["audit_terms"] = len(terms)
			}
		}
	}

	c.JSON(http.StatusOK, resp)
}

// GetSecurityShieldRules handles GET /v0/management/security-shield/rules
func (h *Handler) GetSecurityShieldRules(c *gin.Context) {
	rulesPath := getRulesPath()
	data, err := os.ReadFile(rulesPath)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "rules file not found: " + err.Error()})
		return
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to parse rules JSON: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, parsed)
}

// PostSecurityShieldRules handles POST /v0/management/security-shield/rules
func (h *Handler) PostSecurityShieldRules(c *gin.Context) {
	var body map[string]interface{}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body: " + err.Error()})
		return
	}

	rulesPath := getRulesPath()
	formatted, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to format rules JSON: " + err.Error()})
		return
	}

	// Atomic write
	tmpFile := rulesPath + ".tmp"
	if err := os.WriteFile(tmpFile, formatted, 0644); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to write temp rules: " + err.Error()})
		return
	}

	if err := os.Rename(tmpFile, rulesPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to atomically replace rules: " + err.Error()})
		return
	}

	log.Infof("security_shield rules updated via management API: %s", rulesPath)
	c.JSON(http.StatusOK, gin.H{"status": "ok", "message": "rules updated successfully, hot-reload active in 3s"})
}

// PostSecurityShieldCanary handles POST /v0/management/security-shield/canary
func (h *Handler) PostSecurityShieldCanary(c *gin.Context) {
	scriptPath := "/usr/local/sbin/cliproxy-functional-health.py"
	if _, err := os.Stat(scriptPath); err != nil {
		scriptPath = "scripts/cliproxy-functional-health.py"
	}

	if _, err := os.Stat(scriptPath); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "canary script not found on system"})
		return
	}

	cmd := exec.Command("python3", scriptPath, "--canary")
	out, err := cmd.CombinedOutput()
	outputStr := string(out)

	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"status":  "warning",
			"healthy": false,
			"error":   err.Error(),
			"raw":     outputStr,
		})
		return
	}

	var parsed map[string]interface{}
	if jsonErr := json.Unmarshal(out, &parsed); jsonErr == nil {
		c.JSON(http.StatusOK, gin.H{
			"status":  "ok",
			"healthy": true,
			"data":    parsed,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"raw":    outputStr,
	})
}

// PostSecurityShieldDiagnose handles POST /v0/management/security-shield/diagnose
func (h *Handler) PostSecurityShieldDiagnose(c *gin.Context) {
	diagScript := "/usr/local/sbin/diagnose-prompt.py"
	if _, err := os.Stat(diagScript); err != nil {
		diagScript = filepath.Join("scripts", "diagnose-prompt.py")
	}

	if _, err := os.Stat(diagScript); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "diagnose script not found"})
		return
	}

	cmd := exec.Command("python3", diagScript)
	out, err := cmd.CombinedOutput()

	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"success": err == nil,
		"output":  string(out),
	})
}
