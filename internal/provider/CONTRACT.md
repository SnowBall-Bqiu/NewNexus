# ProviderAdapterV1

Protocol version `1` is a compiled-in, transport-neutral Go contract. An external
adapter process may later implement the same messages over authenticated gRPC;
the server does not load arbitrary binaries or accept a provider base URL through
REST credentials. `AdapterProtocolVersion` and the compile-time interface assertion
freeze the currently implemented boundary.

| Method | Contract |
| --- | --- |
| `Validate(ctx)` | Authenticate, enumerate accessible zones and discover actual capabilities. A successful read does not prove write permission. |
| `Zones(ctx)` | Exhaust all provider pages or return an error. IDs are opaque strings, including DNSPod 64-bit IDs. |
| `Lines(ctx, zone)` | Return actual provider line codes; only empty/`0` map to `default`. Display names do not become API codes. |
| `Records(ctx, zone)` | Exhaust records and return exact provider IDs, FQDN owners, DNS type, value, TTL, line, weight and proxy state. Unknown weight is neutral `1`. |
| `Apply(ctx, zone, changes)` | Validate all change envelopes, execute in order and return the realized record subset even when a later change fails. This operation is not atomic. |
| `Rollback(ctx, zone, completed)` | Reverse only the completed changes carrying realized provider IDs. Deleted records can receive new IDs when recreated. Callers must perform fresh read-back before claiming success. |

`ApplyChanges` supplements `Apply` with the same realized changes and IDs. The
control service persists each completed change before proceeding and uses that
journal for compensation. It compares complete provider state to reviewed
snapshots before apply or rollback, then checks provider read-back and direct
authoritative DNS answers. It refuses to delete unknown IDs or overwrite manual
edits. Weighted/geographic DNS may return a subset of reviewed values; complete
line configuration is verified through the provider API.

`provider.Error` exposes sanitized status/code plus `Retryable` and `Ambiguous`.
Rate limits retry up to four requests respecting bounded Retry-After. Read-only
5xx responses and transient transport failures can retry with bounded exponential
backoff; a cancelled context stops immediately. Mutation transport errors, malformed success responses,
missing returned IDs and 5xx failures are ambiguous and are never blindly retried
inside the adapter. A successful-looking response without a provider ID cannot
be reported as a completed create. Response bodies, tokens, secret keys and
submitted payloads never become public error text. Context cancellation bounds
requests, pagination and backoff.

## Capability degradation

Capabilities are refreshed for the selected zone before every preview/apply.
Cloudflare DNS has only a default line and does not simulate weighted DNS with
duplicate records. Apex CNAME flattening is enabled only for Cloudflare. DNSPod
line codes and TTL/weight limits come from DescribeRecordLineList and actual
DescribeDomainPurview entitlements. AliDNS CNAME weights require an already
enabled matching hostname/type/line SLB policy; policy writes are not guessed
from a plan name. Unsupported apex, lines, weights, proxy modes or TTLs reject
the operation before publication.

Zone permission/API discovery failures are saved in `capability_errors`, leave
the connection `limited`, and keep unavailable operations disabled. SaaS is
available only when the real Cloudflare quota endpoint succeeds. Creating a
custom hostname rechecks quota and lets the real mutation API enforce write
permissions and applicable hard limits; it never reports a fabricated hostname
ID, paid entitlement or successful operation.

Custom origin is available on all Cloudflare for SaaS plans, separately from
Enterprise SNI rewrite. `custom_origin` is enabled only after the real SaaS quota
API succeeds. Creation also verifies that the hostname has an explicit proxied
A/AAAA/CNAME in the same Cloudflare account; missing zone-read permission,
unknown account, wildcard-only and DNS-only records fail before the mutation.
The provider mutation still makes the final permission/entitlement decision.

Cloudflare `allocated` is a soft quota, while `hard_cap` is enforced for
non-Enterprise customers. Enterprise customers may exceed it. Consequently the
server displays used/allocation/threshold/scope and billing warnings, but does
not infer a plan or block every customer locally at a soft/hard threshold.
Pending hostnames consume quota until deleted. Bulk operations use independent
jobs so provider quota failures remain visible for each affected item.

SaaS hostname ownership and SSL certificate state are independent. TXT/HTTP
ownership instructions and certificate DCV instructions are stored separately.
Traffic cutover requires both live provider states to be `active`. An explicit
migration preview records the old customer DNS and a reviewed diff; apply checks
those conditions again, and rollback restores old records without claiming
ownership of previously external DNS. O2O customers already on Cloudflare cannot
use TXT/HTTP ownership pre-validation; the UI must disclose that provider limit.

## Primary API references

- [Cloudflare quota behavior and billing](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/quotas-and-billing/)
- [Cloudflare custom hostname create API](https://developers.cloudflare.com/api/resources/custom_hostnames/methods/create/)
- [Cloudflare plan capabilities](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/plans/)
- [Cloudflare custom origin prerequisites](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/start/advanced-settings/custom-origin/)
- [Cloudflare zero-downtime migration](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/domain-support/hostname-validation/zero-downtime-migration/)
- [Cloudflare pre-validation and O2O restriction](https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/domain-support/hostname-validation/pre-validation/)
- [DNSPod domain entitlements](https://cloud.tencent.com/document/product/1427/56170)
- [DNSPod record list](https://cloud.tencent.com/document/product/1427/56166)
- [AliDNS supported lines](https://www.alibabacloud.com/help/en/dns/api-alidns-2015-01-09-describesupportlines)
- [AliDNS DNS load balancing policies](https://www.alibabacloud.com/help/en/dns/api-alidns-2015-01-09-describednsslbsubdomains)

Contract tests use real HTTP fixture servers and UDP authoritative DNS servers;
they do not assert that a paid third-party production account was exercised.
