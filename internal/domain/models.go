package domain

import "time"

type Model struct {
	ID        string    `gorm:"primaryKey;size:36" json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Admin struct {
	Model
	Username             string     `gorm:"uniqueIndex;size:80" json:"username"`
	PasswordHash         string     `json:"-"`
	TOTPSecretEnc        string     `json:"-"`
	PendingTOTPSecretEnc string     `json:"-"`
	TOTPEnabled          bool       `json:"totpEnabled"`
	RecoveryHashes       string     `json:"-"`
	PasswordChangedAt    *time.Time `json:"passwordChangedAt,omitempty"`
}

type Session struct {
	Model
	AdminID   string     `gorm:"index;size:36" json:"adminId"`
	TokenHash string     `gorm:"uniqueIndex;size:64" json:"-"`
	CSRFHash  string     `gorm:"size:64" json:"-"`
	ExpiresAt time.Time  `gorm:"index" json:"expiresAt"`
	LastSeen  time.Time  `json:"lastSeen"`
	IPAddress string     `json:"ipAddress"`
	UserAgent string     `json:"userAgent"`
	RevokedAt *time.Time `gorm:"index" json:"revokedAt,omitempty"`
}

type APIKey struct {
	Model
	AdminID    string     `gorm:"index;size:36" json:"adminId"`
	Name       string     `json:"name"`
	Prefix     string     `gorm:"index;size:16" json:"prefix"`
	SecretHash string     `json:"-"`
	Scopes     string     `json:"scopes"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	RevokedAt  *time.Time `gorm:"index" json:"revokedAt,omitempty"`
}

type Connection struct {
	Model
	Name              string     `gorm:"index" json:"name"`
	Provider          string     `gorm:"index;size:32" json:"provider"`
	Purposes          string     `json:"purposes"`
	CredentialEnc     string     `json:"-"`
	CredentialMask    string     `json:"credentialMask"`
	Capabilities      string     `json:"capabilities"`
	Status            string     `gorm:"index;size:24" json:"status"`
	StatusMessage     string     `json:"statusMessage"`
	LastValidatedAt   *time.Time `json:"lastValidatedAt,omitempty"`
	DesiredJSON       string     `json:"desiredJson"`
	ObservedJSON      string     `json:"observedJson"`
	ConfigFingerprint string     `gorm:"index;size:64" json:"configFingerprint"`
	LastSyncedAt      *time.Time `json:"lastSyncedAt,omitempty"`
	DriftStatus       string     `gorm:"index;size:24" json:"driftStatus"`
}

type Project struct {
	Model
	Name        string `json:"name"`
	Avatar      string `gorm:"size:16" json:"avatar"`
	Description string `json:"description"`
	Status      string `gorm:"index;size:24" json:"status"`
	Color       string `gorm:"size:16" json:"color"`
}

type ProjectSite struct {
	Model
	ProjectID          string `gorm:"index;size:36" json:"projectId"`
	Name               string `json:"name"`
	URL                string `json:"url"`
	ProbeProfileID     string `gorm:"index;size:36" json:"probeProfileId"`
	ProbeOverridesJSON string `json:"probeOverridesJson"`
	Status             string `gorm:"index;size:24" json:"status"`
}

type SaaSZone struct {
	Model
	ProjectID         string     `gorm:"index;size:36" json:"projectId"`
	ConnectionID      string     `gorm:"index;size:36" json:"connectionId"`
	ZoneID            string     `gorm:"index" json:"zoneId"`
	ZoneName          string     `json:"zoneName"`
	FallbackOrigin    string     `json:"fallbackOrigin"`
	CNAMETarget       string     `json:"cnameTarget"`
	Status            string     `gorm:"index;size:24" json:"status"`
	QuotaAllocated    int        `json:"quotaAllocated"`
	QuotaUsed         int        `json:"quotaUsed"`
	QuotaHardCap      int        `json:"quotaHardCap"`
	QuotaExceeded     bool       `json:"quotaExceeded"`
	FallbackStatus    string     `gorm:"index;size:24" json:"fallbackStatus"`
	CapabilitiesJSON  string     `json:"capabilitiesJson"`
	DesiredJSON       string     `json:"desiredJson"`
	ObservedJSON      string     `json:"observedJson"`
	ConfigFingerprint string     `gorm:"index;size:64" json:"configFingerprint"`
	LastSyncedAt      *time.Time `json:"lastSyncedAt,omitempty"`
	DriftStatus       string     `gorm:"index;size:24" json:"driftStatus"`
}

type CustomHostname struct {
	Model
	SaaSZoneID               string     `gorm:"column:saas_zone_id;index;size:36" json:"saasZoneId"`
	DNSConnectionID          string     `gorm:"index;size:36" json:"dnsConnectionId"`
	DNSZoneID                string     `json:"dnsZoneId"`
	DNSZoneName              string     `json:"dnsZoneName"`
	Hostname                 string     `gorm:"index" json:"hostname"`
	ValidationMethod         string     `json:"validationMethod"`
	AutomationMode           string     `json:"automationMode"`
	DNSAutomationPaused      bool       `gorm:"index" json:"dnsAutomationPaused"`
	DNSAutomationPauseReason string     `json:"dnsAutomationPauseReason"`
	Status                   string     `gorm:"index;size:32" json:"status"`
	SSLStatus                string     `gorm:"index;size:32" json:"sslStatus"`
	OwnershipStatus          string     `gorm:"index;size:32" json:"ownershipStatus"`
	ValidationName           string     `json:"validationName"`
	ValidationValue          string     `json:"validationValue"`
	OwnershipValidationJSON  string     `json:"ownershipValidationJson"`
	SSLValidationJSON        string     `json:"sslValidationJson"`
	CustomOrigin             string     `json:"customOrigin"`
	ProviderID               string     `gorm:"index" json:"providerId"`
	CutoverStatus            string     `gorm:"index;size:24" json:"cutoverStatus"`
	CutoverApprovalRequired  bool       `json:"cutoverApprovalRequired"`
	DesiredJSON              string     `json:"desiredJson"`
	ObservedJSON             string     `json:"observedJson"`
	ConfigFingerprint        string     `gorm:"index;size:64" json:"configFingerprint"`
	DriftStatus              string     `gorm:"index;size:24" json:"driftStatus"`
	ErrorMessage             string     `json:"errorMessage"`
	LastCheckedAt            *time.Time `json:"lastCheckedAt,omitempty"`
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
	P50MS        float64    `json:"p50Ms"`
	P95MS        float64    `json:"p95Ms"`
	FailureRate  float64    `json:"failureRate"`
	SampleCount  int        `json:"sampleCount"`
	DownloadBPS  float64    `json:"downloadBps"`
	NodeGroup    string     `gorm:"index" json:"nodeGroup"`
	LineScope    string     `gorm:"index" json:"lineScope"`
	NextProbeAt  *time.Time `gorm:"index" json:"nextProbeAt,omitempty"`
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
	Name                  string     `json:"name"`
	GroupName             string     `gorm:"index" json:"groupName"`
	Region                string     `gorm:"index" json:"region"`
	Carrier               string     `gorm:"index" json:"carrier"`
	Protocols             string     `json:"protocols"`
	Version               string     `json:"version"`
	Status                string     `gorm:"index;size:24" json:"status"`
	EnrollCodeHash        string     `gorm:"index;size:64" json:"-"`
	EnrollExpiresAt       *time.Time `gorm:"index" json:"enrollExpiresAt,omitempty"`
	RecoveryCodeHash      string     `gorm:"index;size:64" json:"-"`
	RecoveryExpiresAt     *time.Time `gorm:"index" json:"recoveryExpiresAt,omitempty"`
	AgentTokenHash        string     `gorm:"index;size:64" json:"-"`
	PreviousTokenHash     string     `gorm:"index;size:64" json:"-"`
	TokenExpiresAt        *time.Time `gorm:"index" json:"tokenExpiresAt,omitempty"`
	TokenRotateUntil      *time.Time `gorm:"index" json:"tokenRotateUntil,omitempty"`
	PendingTokenHash      string     `gorm:"index;size:64" json:"-"`
	PendingTokenExpiresAt *time.Time `gorm:"index" json:"pendingTokenExpiresAt,omitempty"`
	RotationID            string     `gorm:"index;size:36" json:"rotationId,omitempty"`
	RotationRequestedAt   *time.Time `json:"rotationRequestedAt,omitempty"`
	RevokedAt             *time.Time `gorm:"index" json:"revokedAt,omitempty"`
	ProtocolVersion       string     `gorm:"index;size:24" json:"protocolVersion"`
	FeaturesJSON          string     `json:"featuresJson"`
	CompatibilityStatus   string     `gorm:"index;size:24" json:"compatibilityStatus"`
	LastSeenAt            *time.Time `json:"lastSeenAt,omitempty"`
	LastAddress           string     `json:"lastAddress"`
	TasksCompleted        int64      `json:"tasksCompleted"`
	FailureRate           float64    `json:"failureRate"`
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
	DownloadBPS   float64   `json:"downloadBps"`
	StatusCode    int       `json:"statusCode"`
	ObservedAt    time.Time `gorm:"index" json:"observedAt"`
}

// ProbeSampleReceipt provides global result idempotency even when PostgreSQL
// stores samples in daily partitions whose unique keys must include observed_at.
type ProbeSampleReceipt struct {
	JobID      string    `gorm:"primaryKey;size:160" json:"-"`
	SampleID   string    `gorm:"index;size:36" json:"-"`
	ObservedAt time.Time `gorm:"index" json:"-"`
	CreatedAt  time.Time `json:"-"`
}

type ProbeAggregate struct {
	Model
	Bucket        string    `gorm:"index:idx_probe_aggregate_scope,priority:1;size:12" json:"bucket"`
	BucketStart   time.Time `gorm:"index:idx_probe_aggregate_scope,priority:2" json:"bucketStart"`
	CandidateID   string    `gorm:"index:idx_probe_aggregate_scope,priority:3;size:36" json:"candidateId"`
	RouteTargetID string    `gorm:"index:idx_probe_aggregate_scope,priority:4;size:36" json:"routeTargetId"`
	NodeGroup     string    `gorm:"index:idx_probe_aggregate_scope,priority:5" json:"nodeGroup"`
	LineScope     string    `gorm:"index:idx_probe_aggregate_scope,priority:6" json:"lineScope"`
	Samples       int       `json:"samples"`
	Successes     int       `json:"successes"`
	SuccessRate   float64   `json:"successRate"`
	TCPMS         float64   `json:"tcpMs"`
	TLSMS         float64   `json:"tlsMs"`
	TTFBMS        float64   `json:"ttfbMs"`
	P50MS         float64   `json:"p50Ms"`
	P95MS         float64   `json:"p95Ms"`
	DownloadBPS   float64   `json:"downloadBps"`
}

type Route struct {
	Model
	ProjectID         string     `gorm:"index;size:36" json:"projectId"`
	SiteID            string     `gorm:"index;size:36" json:"siteId"`
	Name              string     `json:"name"`
	Hostname          string     `gorm:"index" json:"hostname"`
	DNSConnectionID   string     `gorm:"index;size:36" json:"dnsConnectionId"`
	ZoneID            string     `json:"zoneId"`
	ZoneName          string     `json:"zoneName"`
	TTL               int        `json:"ttl"`
	PublishMode       string     `json:"publishMode"`
	SwitchMode        string     `json:"switchMode"`
	Status            string     `gorm:"index;size:24" json:"status"`
	ProbeProfileID    string     `json:"probeProfileId"`
	LastReleaseID     string     `json:"lastReleaseId"`
	DesiredJSON       string     `json:"desiredJson"`
	ObservedJSON      string     `json:"observedJson"`
	ConfigFingerprint string     `gorm:"index;size:64" json:"configFingerprint"`
	DriftStatus       string     `gorm:"index;size:24" json:"driftStatus"`
	LastSyncedAt      *time.Time `json:"lastSyncedAt,omitempty"`
	CooldownUntil     *time.Time `json:"cooldownUntil,omitempty"`
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
	AutoSwitchPaused  bool       `gorm:"index" json:"autoSwitchPaused"`
	PauseReason       string     `json:"pauseReason"`
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
	RouteID      string     `gorm:"uniqueIndex:idx_release_route_version,priority:1;size:36" json:"routeId"`
	Version      int        `gorm:"uniqueIndex:idx_release_route_version,priority:2" json:"version"`
	Trigger      string     `json:"trigger"`
	Status       string     `gorm:"index;size:24" json:"status"`
	DiffJSON     string     `json:"diffJson"`
	SnapshotJSON string     `json:"snapshotJson"`
	ErrorMessage string     `json:"errorMessage"`
	AppliedAt    *time.Time `json:"appliedAt,omitempty"`
	RolledBackAt *time.Time `json:"rolledBackAt,omitempty"`
}

type ManagedDNSRecord struct {
	Model
	ConnectionID     string     `gorm:"index:idx_managed_dns_scope,priority:1;size:36" json:"connectionId"`
	ZoneID           string     `gorm:"index:idx_managed_dns_scope,priority:2" json:"zoneId"`
	ResourceType     string     `gorm:"index:idx_managed_dns_scope,priority:3;size:32" json:"resourceType"`
	ResourceID       string     `gorm:"index:idx_managed_dns_scope,priority:4;size:36" json:"resourceId"`
	ProviderRecordID string     `gorm:"index" json:"providerRecordId"`
	Name             string     `gorm:"index" json:"name"`
	Type             string     `gorm:"size:12" json:"type"`
	Value            string     `json:"value"`
	LineCode         string     `json:"lineCode"`
	TTL              int        `json:"ttl"`
	Weight           int        `json:"weight"`
	Fingerprint      string     `gorm:"index;size:64" json:"fingerprint"`
	ObservedJSON     string     `json:"observedJson"`
	LastVerifiedAt   *time.Time `json:"lastVerifiedAt,omitempty"`
	DriftStatus      string     `gorm:"index;size:24" json:"driftStatus"`
}

type Alert struct {
	Model
	Severity       string     `gorm:"index;size:16" json:"severity"`
	Category       string     `gorm:"index;size:32" json:"category"`
	Title          string     `json:"title"`
	Message        string     `json:"message"`
	ResourceType   string     `json:"resourceType"`
	ResourceID     string     `gorm:"index" json:"resourceId"`
	DedupeKey      string     `gorm:"index" json:"dedupeKey"`
	Status         string     `gorm:"index;size:16" json:"status"`
	AcknowledgedAt *time.Time `json:"acknowledgedAt,omitempty"`
	ResolvedAt     *time.Time `json:"resolvedAt,omitempty"`
	SilencedUntil  *time.Time `gorm:"index" json:"silencedUntil,omitempty"`
	Occurrences    int        `json:"occurrences"`
	LastOccurredAt *time.Time `json:"lastOccurredAt,omitempty"`
}

type NotificationChannel struct {
	Model
	Name       string     `json:"name"`
	Type       string     `gorm:"index;size:16" json:"type"`
	Enabled    bool       `json:"enabled"`
	ConfigJSON string     `json:"configJson"`
	SecretEnc  string     `json:"-"`
	LastTestAt *time.Time `json:"lastTestAt,omitempty"`
	LastError  string     `json:"lastError"`
}

type NotificationDelivery struct {
	Model
	AlertID       string     `gorm:"index;size:36" json:"alertId"`
	ChannelID     string     `gorm:"index;size:36" json:"channelId"`
	EventID       string     `gorm:"uniqueIndex;size:36" json:"eventId"`
	Status        string     `gorm:"index;size:16" json:"status"`
	Attempts      int        `json:"attempts"`
	NextAttemptAt *time.Time `gorm:"index" json:"nextAttemptAt,omitempty"`
	LastError     string     `json:"lastError"`
	DeliveredAt   *time.Time `json:"deliveredAt,omitempty"`
}

type Job struct {
	Model
	Type           string     `gorm:"index;size:64" json:"type"`
	Status         string     `gorm:"index;size:20" json:"status"`
	ResourceType   string     `gorm:"index;size:32" json:"resourceType"`
	ResourceID     string     `gorm:"index;size:36" json:"resourceId"`
	ExecutionKey   string     `gorm:"index;size:128" json:"executionKey"`
	PayloadJSON    string     `json:"payloadJson"`
	ResultJSON     string     `json:"resultJson"`
	ErrorCode      string     `json:"errorCode"`
	ErrorMessage   string     `json:"errorMessage"`
	Progress       int        `json:"progress"`
	Attempts       int        `json:"attempts"`
	MaxAttempts    int        `json:"maxAttempts"`
	IdempotencyKey string     `gorm:"uniqueIndex;size:160" json:"idempotencyKey"`
	RunAt          time.Time  `gorm:"index" json:"runAt"`
	LeaseOwner     string     `gorm:"index;size:64" json:"leaseOwner"`
	LeaseUntil     *time.Time `gorm:"index" json:"leaseUntil,omitempty"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	FinishedAt     *time.Time `json:"finishedAt,omitempty"`
	CancelledAt    *time.Time `json:"cancelledAt,omitempty"`
	WaitingForLock bool       `gorm:"-" json:"waitingForLock"`
	LockOwnerJobID string     `gorm:"-" json:"lockOwnerJobId,omitempty"`
	ExecutionOrder int        `gorm:"-" json:"executionOrder,omitempty"`
}

type JobAttempt struct {
	Model
	JobID        string     `gorm:"index;size:36" json:"jobId"`
	Attempt      int        `json:"attempt"`
	Worker       string     `json:"worker"`
	Status       string     `gorm:"index;size:20" json:"status"`
	ErrorCode    string     `json:"errorCode"`
	ErrorMessage string     `json:"errorMessage"`
	StartedAt    time.Time  `json:"startedAt"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
}

// JobExecutionLease serializes external writes for one resource across workers.
type JobExecutionLease struct {
	ExecutionKey string    `gorm:"primaryKey;size:128" json:"executionKey"`
	JobID        string    `gorm:"index;size:36" json:"jobId"`
	LeaseOwner   string    `gorm:"index;size:64" json:"leaseOwner"`
	ExpiresAt    time.Time `gorm:"index" json:"expiresAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type AgentTask struct {
	Model
	JobID          string     `gorm:"index;size:36" json:"jobId"`
	NodeID         string     `gorm:"index;size:36" json:"nodeId"`
	CandidateID    string     `gorm:"index;size:36" json:"candidateId"`
	RouteTargetID  string     `gorm:"index;size:36" json:"routeTargetId"`
	Kind           string     `gorm:"index;size:32" json:"kind"`
	PayloadJSON    string     `json:"payloadJson"`
	Status         string     `gorm:"index;size:20" json:"status"`
	Attempt        int        `json:"attempt"`
	LeaseUntil     *time.Time `gorm:"index" json:"leaseUntil,omitempty"`
	AcknowledgedAt *time.Time `json:"acknowledgedAt,omitempty"`
	CompletedAt    *time.Time `json:"completedAt,omitempty"`
	ResultCursor   int64      `json:"resultCursor"`
}

type InstanceLease struct {
	Name      string    `gorm:"primaryKey;size:64" json:"name"`
	Owner     string    `gorm:"index;size:64" json:"owner"`
	ExpiresAt time.Time `gorm:"index" json:"expiresAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type SchemaMigration struct {
	Version   int64     `gorm:"primaryKey" json:"version"`
	Name      string    `json:"name"`
	AppliedAt time.Time `json:"appliedAt"`
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
