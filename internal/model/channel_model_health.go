package model

import "time"

type ChannelModelHealthStatus string

const (
	ChannelModelHealthQueued      ChannelModelHealthStatus = "queued"
	ChannelModelHealthRunning     ChannelModelHealthStatus = "running"
	ChannelModelHealthSuccess     ChannelModelHealthStatus = "success"
	ChannelModelHealthFailed      ChannelModelHealthStatus = "failed"
	ChannelModelHealthStale       ChannelModelHealthStatus = "stale"
	ChannelModelHealthInterrupted ChannelModelHealthStatus = "interrupted"
)

type ChannelModelHealth struct {
	ID                int                      `json:"id" gorm:"primaryKey"`
	ChannelID         int                      `json:"channel_id" gorm:"uniqueIndex:idx_channel_model_health_key"`
	ModelName         string                   `json:"model_name" gorm:"uniqueIndex:idx_channel_model_health_key"`
	Status            ChannelModelHealthStatus `json:"status" gorm:"index"`
	HTTPStatus        int                      `json:"http_status"`
	DurationMS        int64                    `json:"duration_ms"`
	ErrorMessage      string                   `json:"error_message"`
	ChannelKeyID      int                      `json:"channel_key_id"`
	KeyRemark         string                   `json:"key_remark"`
	CheckedAt         *time.Time               `json:"checked_at"`
	ConfigFingerprint string                   `json:"config_fingerprint"`
	CreatedAt         time.Time                `json:"created_at"`
	UpdatedAt         time.Time                `json:"updated_at"`
}

type ChannelModelHealthTarget struct {
	ChannelID int    `json:"channel_id" binding:"required"`
	ModelName string `json:"model_name" binding:"required"`
}

type ChannelModelHealthRunRequest struct {
	Targets []ChannelModelHealthTarget `json:"targets" binding:"required"`
}

type ChannelModelHealthRunAccepted struct {
	TaskID string `json:"task_id"`
	Count  int    `json:"count"`
}

type ChannelModelGroupCandidate struct {
	GroupID   int    `json:"group_id"`
	GroupName string `json:"group_name"`
	Reason    string `json:"reason"`
}

type ChannelModelGroupPreviewItem struct {
	ChannelID        int                          `json:"channel_id"`
	ModelName        string                       `json:"model_name"`
	Health           *ChannelModelHealth          `json:"health,omitempty"`
	ExistingGroupIDs []int                        `json:"existing_group_ids"`
	ExcludedGroupIDs []int                        `json:"excluded_group_ids"`
	Candidates       []ChannelModelGroupCandidate `json:"candidates"`
}

type ChannelModelGroupPreviewRequest struct {
	Targets []ChannelModelHealthTarget `json:"targets" binding:"required"`
}

type ChannelModelGroupApplyItem struct {
	ChannelID       int    `json:"channel_id" binding:"required"`
	ModelName       string `json:"model_name" binding:"required"`
	GroupID         *int   `json:"group_id,omitempty"`
	CreateGroupName string `json:"create_group_name,omitempty"`
	ForceUnhealthy  bool   `json:"force_unhealthy"`
}

type ChannelModelGroupApplyRequest struct {
	Items []ChannelModelGroupApplyItem `json:"items" binding:"required"`
}

type ChannelModelGroupApplyResult struct {
	Added         int      `json:"added"`
	Existing      int      `json:"existing"`
	Excluded      int      `json:"excluded"`
	Skipped       int      `json:"skipped"`
	CreatedGroups int      `json:"created_groups"`
	Failed        []string `json:"failed"`
}
