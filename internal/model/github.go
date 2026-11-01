package model

import "time"

// GitHubIssueNotification durably tracks the acknowledgement for one queued issue.
type GitHubIssueNotification struct {
	ID             uint   `gorm:"primaryKey"`
	EventID        uint   `gorm:"uniqueIndex"`
	Repository     string `gorm:"size:255"`
	IssueNumber    int
	Token          string `json:"-"`
	TokenSecretRef string `gorm:"size:255"`
	TaskURL        string
	Attempts       int
	AvailableAt    time.Time  `gorm:"index"`
	DeliveredAt    *time.Time `gorm:"index"`
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
