package configcenter

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"gorm.io/gorm"
)

// DatabaseConfig is supplied by the process environment because the database
// must be known before persisted settings can be read.
type DatabaseConfig struct {
	Driver string
	DSN    string
}

func DatabaseConfigFromEnv() DatabaseConfig {
	cfg := DatabaseConfig{Driver: "sqlite3", DSN: "config.db"}
	if value := os.Getenv("RAFT_DB_DRIVER"); value != "" {
		cfg.Driver = value
	}
	if value := os.Getenv("RAFT_DB_DSN"); value != "" {
		cfg.DSN = value
	}
	return cfg
}

// SystemSetting is defined in models.go.

type RuntimeConfig struct {
	LocalIP   string
	NodeIPs   []string
	AuthToken string
	// GeneratedToken is true when the admin token was randomly generated at
	// startup because RAFT_AUTH_TOKEN was not provided.
	GeneratedToken bool
}

// LoadOrInitializeRuntimeConfig loads settings from the database. On the first
// run it persists node settings. The admin token comes from RAFT_AUTH_TOKEN
// when set; otherwise it is read from the database, and only generated (and
// persisted) the very first time so it stays stable across restarts.
func (s *Store) LoadOrInitializeRuntimeConfig() (RuntimeConfig, error) {
	var setting SystemSetting
	if err := s.db.First(&setting, 1).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		localIP := strings.TrimSpace(os.Getenv("RAFT_LOCAL_IP"))
		if localIP == "" {
			localIP = "127.0.0.1"
		}
		nodeIPs := splitNodeIPs(os.Getenv("RAFT_NODE_IPS"))
		encoded, marshalErr := json.Marshal(nodeIPs)
		if marshalErr != nil {
			return RuntimeConfig{}, marshalErr
		}
		setting = SystemSetting{ID: 1, LocalIP: localIP, NodeIPs: string(encoded)}
		if err := s.db.Create(&setting).Error; err != nil {
			return RuntimeConfig{}, fmt.Errorf("initialize system settings: %w", err)
		}
	} else if err != nil {
		return RuntimeConfig{}, fmt.Errorf("load system settings: %w", err)
	}

	var nodeIPs []string
	if err := json.Unmarshal([]byte(setting.NodeIPs), &nodeIPs); err != nil {
		return RuntimeConfig{}, fmt.Errorf("decode node IPs from database: %w", err)
	}

	// 管理员 token：优先使用环境变量；否则复用数据库中已保存的 token；
	// 只有首次运行时才随机生成一个短 token（uuid 前面一段）并落库，
	// 这样重启后 token 保持不变。
	generatedToken := false
	token := strings.TrimSpace(os.Getenv("RAFT_AUTH_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(setting.AuthToken)
	}
	if token == "" {
		var tokenErr error
		token, tokenErr = newAdminToken()
		if tokenErr != nil {
			return RuntimeConfig{}, fmt.Errorf("generate admin token: %w", tokenErr)
		}
		generatedToken = true
		if err := s.db.Model(&setting).Update("auth_token", token).Error; err != nil {
			return RuntimeConfig{}, fmt.Errorf("persist admin token: %w", err)
		}
	}
	return RuntimeConfig{
		LocalIP:        setting.LocalIP,
		NodeIPs:        nodeIPs,
		AuthToken:      token,
		GeneratedToken: generatedToken,
	}, nil
}

func splitNodeIPs(value string) []string {
	if strings.TrimSpace(value) == "" {
		return []string{}
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			result = append(result, value)
		}
	}
	return result
}
