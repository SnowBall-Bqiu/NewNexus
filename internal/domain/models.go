package domain

import "time"

type Model struct {
	ID        string    `gorm:"primaryKey;size:36" json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Admin struct {
	Model
	Username       string `gorm:"uniqueIndex;size:80" json:"username"`
	PasswordHash   string `json:"-"`
	TOTPSecretEnc  string `json:"-"`
	TOTPEnabled    bool   `json:"totpEnabled"`
	RecoveryHashes string `json:"-"`
}

type Session struct {
	Model
	AdminID   string    `gorm:"index;size:36" json:"adminId"`
	TokenHash string    `gorm:"uniqueIndex;size:64" json:"-"`
	CSRFHash  string    `gorm:"size:64" json:"-"`
	ExpiresAt time.Time `gorm:"index" json:"expiresAt"`
	LastSeen  time.Time `json:"lastSeen"`
}

type APIKey struct {
	Model
	Name       string     `json:"name"`
	Prefix     string     `gorm:"index;size:16" json:"prefix"`
	SecretHash string     `json:"-"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
}

type Connection struct {
	Model
	Name            string     `gorm:"index" json:"name"`
	Provider        string     `gorm:"index;size:32" json:"provider"`
	Purposes        string     `json:"purposes"`
	CredentialEnc   string     `json:"-"`
	CredentialMask  string     `json:"credentialMask"`
	Capabilities    string     `json:"capabilities"`
	Status          string     `gorm:"index;size:24" json:"status"`
	StatusMessage   string     `json:"statusMessage"`
	LastValidatedAt *time.Time `json:"lastValidatedAt,omitempty"`
}

type Project struct {
	Model
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `gorm:"index;size:24" json:"status"`
	Color       string `gorm:"size:16" json:"color"`
}

type SaaSZone struct {
	Model
	ProjectID      string `gorm:"index;size:36" json:"projectId"`
	ConnectionID   string `gorm:"index;size:36" json:"connectionId"`
	ZoneID         string `gorm:"index" json:"zoneId"`
	ZoneName       string `json:"zoneName"`
	FallbackOrigin string `json:"fallbackOrigin"`
	CNAMETarget    string `json:"cnameTarget"`
	Status         string `gorm:"index;size:24" json:"status"`
	QuotaAllocated int    `json:"quotaAllocated"`
	QuotaUsed      int    `json:"quotaUsed"`
	QuotaHardCap   int    `json:"quotaHardCap"`
	ObservedJSON   string `json:"observedJson"`
}

type CustomHostname struct {
	Model
	SaaSZoneID       string     `gorm:"index;size:36" json:"saasZoneId"`
	DNSConnectionID  string     `gorm:"index;size:36" json:"dnsConnectionId"`
	DNSZoneID        string     `json:"dnsZoneId"`
	DNSZoneName      string     `json:"dnsZoneName"`
	Hostname         string     `gorm:"index" json:"hostname"`
	ValidationMethod string     `json:"validationMethod"`
	AutomationMode   string     `json:"automationMode"`
	Status           string     `gorm:"index;size:32" json:"status"`
	SSLStatus        string     `gorm:"index;size:32" json:"sslStatus"`
	ValidationName   string     `json:"validationName"`
	ValidationValue  string     `json:"validationValue"`
	CustomOrigin     string     `json:"customOrigin"`
	ProviderID       string     `gorm:"index" json:"providerId"`
	ErrorMessage     string     `json:"errorMessage"`
	LastCheckedAt    *time.Time `json:"lastCheckedAt,omitempty"`
}

type CandidateSource struct {
	Model
	ProjectID      string     `gorm:"index;size:36" json:"projectId"`
	Name           string     `json:"name"`
	Type           string     `gorm:"index;size:32" json:"type"`
	Location       string     `json:"location"`
	Enabled        bool       `json:"enabled"`
	ConfigJSON     string     `json:"configJson"`
	LastSyncedAt   *time.Time `json:"lastSyncedAt,omitempty"`
	CandidateCount int        `json:"candidateCount"`
	Status         string     `json:"status"`
}

type Candidate struct {
	Model
	ProjectID    string     `gorm:"index:idx_candidate_project_address,priority:1;size:36" json:"projectId"`
	SourceID     string     `gorm:"index;size:36" json:"sourceId"`
	Address      string     `gorm:"index:idx_candidate_project_address,unique,priority:2" json:"address"`
	Family       int        `gorm:"index" json:"family"`
	Tier         string     `gorm:"index;size:16" json:"tier"`
	Tags         string     `json:"tags"`
	Healthy      bool       `gorm:"index" json:"healthy"`
	Score        float64    `gorm:"index" json:"score"`
	SuccessRate  float64    `json:"successRate"`
	TCPMS        float64    `json:"tcpMs"`
	TLSMS        float64    `json:"tlsMs"`
	TTFBMS       float64    `json:"ttfbMs"`
	DownloadBPS  float64    `json:"downloadBps"`
	LastProbedAt *time.Time `json:"lastProbedAt,omitempty"`
}

type OptimizedHostname struct {
	Model
	ProjectID       string     `gorm:"index;size:36" json:"projectId"`
	DNSConnectionID string     `gorm:"index;size:36" json:"dnsConnectionId"`
	ZoneID          string     `json:"zoneId"`
	ZoneName        string     `json:"zoneName"`
	Hostname        string     `gorm:"uniqueIndex" json:"hostname"`
	Family          int        `json:"family"`
	TopN            int        `json:"topN"`
	TTL             int        `json:"ttl"`
	Status          string     `gorm:"index;size:24" json:"status"`
	ObservedJSON    string     `json:"observedJson"`
	LastError       string     `json:"lastError"`
	LastPublishedAt *time.Time `json:"lastPublishedAt,omitempty"`
}

type ProbeProfile struct {
	Model
	ProjectID        string `gorm:"index;size:36" json:"projectId"`
	Name             string `json:"name"`
	Scope            string `gorm:"index;size:24" json:"scope"`
	Mode             string `json:"mode"`
	URL              string `json:"url"`
	Host             string `json:"host"`
	SNI              string `json:"sni"`
	Method           string `json:"method"`
	HeadersJSON      string `json:"headersJson"`
	ExpectedStatuses string `json:"expectedStatuses"`
	ExpectedContains string `json:"expectedContains"`
	TimeoutMS        int    `json:"timeoutMs"`
	MaxBytes         int64  `json:"maxBytes"`
	IntervalSeconds  int    `json:"intervalSeconds"`
	UserAgent        string `json:"userAgent"`
}

type ProbeNode struct {
	Model
	Name           string     `json:"name"`
	GroupName      string     `gorm:"index" json:"groupName"`
	Region         string     `gorm:"index" json:"region"`
	Carrier        string     `gorm:"index" json:"carrier"`
	Protocols      string     `json:"protocols"`
	Version        string     `json:"version"`
	Status         string     `gorm:"index;size:24" json:"status"`
	EnrollCodeHash string     `gorm:"index;size:64" json:"-"`
	AgentTokenHash string     `gorm:"index;size:64" json:"-"`
	LastSeenAt     *time.Time `json:"lastSeenAt,omitempty"`
	LastAddress    string     `json:"lastAddress"`
	TasksCompleted int64      `json:"tasksCompleted"`
	FailureRate    float64    `json:"failureRate"`
}

type ProbeSample struct {
	Model
	JobID         string    `gorm:"uniqueIndex" json:"jobId"`
	NodeID        string    `gorm:"index;size:36" json:"nodeId"`
	CandidateID   string    `gorm:"index;size:36" json:"candidateId"`
	RouteTargetID string    `gorm:"index;size:36" json:"routeTargetId"`
	Mode          string    `json:"mode"`
	Success       bool      `gorm:"index" json:"success"`
	ErrorCode     string    `json:"errorCode"`
	TCPMS         float64   `json:"tcpMs"`
	TLSMS         float64   `json:"tlsMs"`
	TTFBMS        float64   `json:"ttfbMs"`
	TotalMS       float64   `json:"totalMs"`
	BytesRead     int64     `json:"bytesRead"`
	StatusCode    int       `json:"statusCode"`
	ObservedAt    time.Time `gorm:"index" json:"observedAt"`
}

type Route struct {
	Model
	ProjectID       string     `gorm:"index;size:36" json:"projectId"`
	Name            string     `json:"name"`
	Hostname        string     `gorm:"index" json:"hostname"`
	DNSConnectionID string     `gorm:"index;size:36" json:"dnsConnectionId"`
	ZoneID          string     `json:"zoneId"`
	ZoneName        string     `json:"zoneName"`
	TTL             int        `json:"ttl"`
	PublishMode     string     `json:"publishMode"`
	SwitchMode      string     `json:"switchMode"`
	Status          string     `gorm:"index;size:24" json:"status"`
	ProbeProfileID  string     `json:"probeProfileId"`
	LastReleaseID   string     `json:"lastReleaseId"`
	CooldownUntil   *time.Time `json:"cooldownUntil,omitempty"`
}

type RouteLine struct {
	Model
	RouteID           string     `gorm:"index;size:36" json:"routeId"`
	ProviderLineCode  string     `json:"providerLineCode"`
	Alias             string     `json:"alias"`
	NodeGroup         string     `json:"nodeGroup"`
	HealthPerspective string     `json:"healthPerspective"`
	MinNodes          int        `json:"minNodes"`
	QuorumPercent     int        `json:"quorumPercent"`
	Status            string     `json:"status"`
	ActivePriority    int        `json:"activePriority"`
	PendingPriority   int        `json:"pendingPriority"`
	ConsecutiveBetter int        `json:"consecutiveBetter"`
	LastEvaluatedAt   *time.Time `json:"lastEvaluatedAt,omitempty"`
}

type RouteTarget struct {
	Model
	RouteLineID         string     `gorm:"index;size:36" json:"routeLineId"`
	Name                string     `json:"name"`
	CNAME               string     `json:"cname"`
	Priority            int        `json:"priority"`
	Weight              int        `json:"weight"`
	Enabled             bool       `json:"enabled"`
	Health              string     `gorm:"index;size:24" json:"health"`
	Score               float64    `json:"score"`
	ConsecutiveFailures int        `json:"consecutiveFailures"`
	ConsecutiveHealthy  int        `json:"consecutiveHealthy"`
	ProbeOverrideJSON   string     `json:"probeOverrideJson"`
	LastCheckedAt       *time.Time `json:"lastCheckedAt,omitempty"`
}

type Release struct {
	Model
	RouteID      string     `gorm:"index;size:36" json:"routeId"`
	Version      int        `json:"version"`
	Trigger      string     `json:"trigger"`
	Status       string     `gorm:"index;size:24" json:"status"`
	DiffJSON     string     `json:"diffJson"`
	SnapshotJSON string     `json:"snapshotJson"`
	ErrorMessage string     `json:"errorMessage"`
	AppliedAt    *time.Time `json:"appliedAt,omitempty"`
	RolledBackAt *time.Time `json:"rolledBackAt,omitempty"`
}

type Alert struct {
	Model
	Severity       string     `gorm:"index;size:16" json:"severity"`
	Category       string     `gorm:"index;size:32" json:"category"`
	Title          string     `json:"title"`
	Message        string     `json:"message"`
	ResourceType   string     `json:"resourceType"`
	ResourceID     string     `gorm:"index" json:"resourceId"`
	Status         string     `gorm:"index;size:16" json:"status"`
	AcknowledgedAt *time.Time `json:"acknowledgedAt,omitempty"`
}

type AuditEvent struct {
	Model
	Action       string `gorm:"index" json:"action"`
	ResourceType string `gorm:"index" json:"resourceType"`
	ResourceID   string `gorm:"index" json:"resourceId"`
	Summary      string `json:"summary"`
	DetailsJSON  string `json:"detailsJson"`
	Actor        string `json:"actor"`
	IPAddress    string `json:"ipAddress"`
}

type Setting struct {
	Key       string    `gorm:"primaryKey" json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updatedAt"`
}
