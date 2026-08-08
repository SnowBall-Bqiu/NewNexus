export type Status = 'healthy' | 'connected' | 'active' | 'online' | 'applied' | 'synced' | 'warning' | 'degraded' | 'recovering' | 'pending' | 'offline' | 'error' | 'failed' | 'unknown' | 'preview';

export interface Model { id: string; createdAt: string; updatedAt: string }
export interface Project extends Model { name: string; description: string; status: Status; color: string }
export interface Connection extends Model { name: string; provider: 'cloudflare' | 'dnspod' | 'alidns'; purposes: string; credentialMask: string; capabilities: string; status: Status; statusMessage: string; lastValidatedAt?: string }
export interface SaaSZone extends Model { projectId: string; connectionId: string; zoneId: string; zoneName: string; fallbackOrigin: string; cnameTarget: string; status: Status; quotaAllocated: number; quotaUsed: number; quotaHardCap: number }
export interface CustomHostname extends Model { saasZoneId: string; dnsConnectionId: string; dnsZoneId: string; dnsZoneName: string; hostname: string; validationMethod: string; automationMode: string; status: Status; sslStatus: Status | 'pending_validation'; validationName: string; validationValue: string; customOrigin: string; errorMessage: string; lastCheckedAt?: string }
export interface CandidateSource extends Model { projectId: string; name: string; type: string; location: string; enabled: boolean; candidateCount: number; status: Status; lastSyncedAt?: string }
export interface Candidate extends Model { projectId: string; sourceId: string; address: string; family: number; tier: string; tags: string; healthy: boolean; score: number; successRate: number; tcpMs: number; tlsMs: number; ttfbMs: number; downloadBps: number; lastProbedAt?: string }
export interface OptimizedHostname extends Model { projectId: string; dnsConnectionId: string; zoneId: string; zoneName: string; hostname: string; family: number; topN: number; ttl: number; status: Status; observedJson: string; lastError: string; lastPublishedAt?: string }
export interface OptimizedHostnamePreview { hostname: string; recordType: 'A' | 'AAAA'; addresses: string[]; changes: { operation: string; before?: DNSRecord; after?: DNSRecord }[]; warnings: string[]; canApply: boolean }
export interface ProbeProfile extends Model { projectId: string; name: string; scope: string; mode: string; url: string; host: string; sni: string; method: string; headersJson: string; expectedStatuses: string; expectedContains: string; timeoutMs: number; maxBytes: number; intervalSeconds: number; userAgent: string }
export interface ProbeNode extends Model { name: string; groupName: string; region: string; carrier: string; protocols: string; version: string; status: Status; lastSeenAt?: string; tasksCompleted: number; failureRate: number }
export interface Route extends Model { projectId: string; name: string; hostname: string; dnsConnectionId: string; zoneId: string; zoneName: string; ttl: number; publishMode: string; switchMode: string; status: Status; probeProfileId: string; lastReleaseId: string; cooldownUntil?: string }
export interface RouteLine extends Model { routeId: string; providerLineCode: string; alias: string; nodeGroup: string; healthPerspective: string; minNodes: number; quorumPercent: number; status: Status; activePriority: number; pendingPriority: number; consecutiveBetter: number; lastEvaluatedAt?: string }
export interface RouteTarget extends Model { routeLineId: string; name: string; cname: string; priority: number; weight: number; enabled: boolean; health: Status; score: number; consecutiveFailures: number; consecutiveHealthy: number; lastCheckedAt?: string }
export interface RouteLineDetail extends RouteLine { targets: RouteTarget[] }
export interface RouteDetail extends Route { lines: RouteLineDetail[] }
export interface Release extends Model { routeId: string; version: number; trigger: string; status: Status; diffJson: string; snapshotJson: string; errorMessage: string; appliedAt?: string; rolledBackAt?: string }
export interface Alert extends Model { severity: 'info' | 'warning' | 'critical'; category: string; title: string; message: string; resourceType: string; resourceId: string; status: string; acknowledgedAt?: string }
export interface AuditEvent extends Model { action: string; resourceType: string; resourceId: string; summary: string; detailsJson: string; actor: string; ipAddress: string }
export interface Setting { key: string; value: string; updatedAt?: string }
export interface Summary { counts: Record<string, number>; healthyRoutes: number; onlineNodes: number; latencySeries: { time: string; latency: number; success: number }[]; updatedAt: string }
export interface RoutePreview { routeId: string; hostname: string; provider: string; canApply: boolean; warnings: string[]; changes: { operation: string; before?: DNSRecord; after?: DNSRecord }[] }
export interface DNSRecord { id?: string; name: string; type: string; value: string; lineCode: string; ttl: number; weight: number; enabled: boolean }
