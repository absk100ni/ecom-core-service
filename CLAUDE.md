# CLAUDE.md

Guidance for Claude Code (and humans) working in this repository. Read this before making changes.

> **Recent hardening (changelog).** The following were implemented after the initial audit — the assessments in §5/§6 below have been updated to match, but flagging here for quick orientation:
> - **v3.2: Category Hierarchy + Search Suggest + DB Design** — 3-level category hierarchy (level, path materialized-path fields, cycle/depth validation, descendant-inclusive product listing), `GET /categories/tree` nested JSON endpoint, `GET /search/suggest?q=` hybrid prefix+text autocomplete (8 results, cached 60s), comprehensive `DATABASE_DESIGN.md` documenting all collections/indexes/scaling.
> - **v3.0: Google Sign-In** — `POST /auth/google` with server-side tokeninfo verification, dev mock (`mock-google:<email>`), account linking via `link-phone`/`verify-phone`.
> - **v3.0: Partial Payment** — advance online + remainder COD. Configurable per-product `advance_percent`, max-across-items rule, ceil rounding. Payment creation charges only the advance.
> - **v3.0: Shipmozo Courier Integration** — replaced Shiprocket with CourierProvider interface (`internal/shipping/provider.go`). Mock provider for dev, Shipmozo for prod. Admin ship endpoint is idempotent.
> - **v3.0: S3 Upload (presigned PUT)** — `POST /admin/uploads/presign` with content_type validation. AWS SDK integration placeholder (network unavailable for `go get`); `/tmp` local fallback for dev.
> - **v3.0: Dedicated Admin Auth** — `POST /admin/auth/login` with bcrypt + 24h JWT (role=admin claim). `AdminAuthMiddleware` REQUIRES role claim — user tokens rejected. Rate-limited login (5/min/IP).
> - **v3.0: Observability** — RequestID middleware (X-Request-ID), access-log middleware (method/path/status/latency/request_id/user_id). Inline admin handlers refactored to `internal/admin`.
> - **v3.0: Tests** — table-driven tests for advance/COD computation, webhook signature verification (Razorpay hex + Cashfree base64), Google token parsing, AdminAuthMiddleware role enforcement, mock provider determinism.
> - **Payment webhooks now verify gateway signatures** (Razorpay HMAC-hex, Cashfree HMAC-base64 over `timestamp+body`) against the raw request body, and **fail closed in production** if the secret is unset.
> - **Payment transitions are idempotent** — `markPaymentPaid` only moves an order `pending → paid` and returns `firstTransition` so confirmation SMS fires once.
> - **Cashfree is the primary gateway** (`PAYMENT_GATEWAY=cashfree` default); `activeGateway()` no longer silently swaps gateways in production.
> - **Shipping webhook** moved out of `main.go` into `internal/shipping`, authenticated via `x-api-key` (constant-time compare), fail-closed in prod.
> - **Production startup guards**: hard-fail on default/empty `JWT_SECRET`, warn on wildcard CORS.
> - **Redis cache layer added** ([pkg/cache](pkg/cache/cache.go)) with graceful degradation; consumers: pincode serviceability/EDD, product listings (60s TTL), categories (5m TTL, with write-invalidation), and the rate limiter.
> - **Admin analytics rewritten** as MongoDB aggregations (`$facet`/`$group`) — no more load-all-into-memory + bubble sort.
> - **Rate limiting is now Redis-backed** (shared fixed-window across replicas), falling back to the per-process limiter if Redis is down — unblocks running >1 replica.
> - **v3.1: Stripe as primary gateway** — `PAYMENT_GATEWAY=stripe` default. Stripe PaymentIntent create/verify/webhook/refund via stdlib `net/http` (no SDK). Cashfree/Razorpay code intact, wired off default. Webhook detects gateway via `Stripe-Signature` header. Signature verification uses constant-time compare with 5min replay tolerance. Dev mock verify path preserved.
> - **v3.1.1 fixes**: (a) users `phone` index is now a **partial unique index** (`phone > ""`) — the old unique+sparse index collided on the empty-string phones of Google-only accounts, limiting the store to ONE Google user; the legacy `phone_1` index is dropped at startup. (b) **Refunds cap at the amount actually paid online** (`advance_amount` for partial-plan orders), never `order.Total`. (c) Mock gateway verify branch added (dev-only, rejected in production). (d) Tracking `events` normalizes nil → `[]`.
> - **v3.2 launch-gaps**: notify service in `internal/notify` (order confirmed/shipped/refund + contact emails; log-only mock when `SMTP_HOST` unset), GST invoices in `internal/invoice` (`INV-<FY>-<seq>` via atomic counter, idempotent; GST-inclusive extraction, CGST/SGST intra-state vs IGST), returns (`internal/returns`, delivered-only + `RETURN_WINDOW_DAYS`, one open per order) and contact (`internal/contact`, rate-limited) APIs. Notifier hooks wired in `markPaymentPaid` (fires once via firstTransition — single funnel for verify + all webhooks), `ProcessRefund`, `CreateShipment`. List response keys: `return_requests`, `contact_messages`. Gotchas: admin login is `/admin/auth/login`, cart add is `/cart/add`.
> - **v3.2.1 Sentry**: stdlib Sentry Store-API client in `internal/sentry` (no SDK). Production+DSN-gated; captures ONLY panics (SentryRecoveryMiddleware) + 5xx (access-log middleware) — never 4xx; hard cap 5 events/hour + 1-hour signature dedupe (free-tier protection). Both frontends: @sentry/react, prod+DSN-gated, tracesSampleRate 0, no Replay, 10 events/session cap.
> - **v3.3 WhatsApp**: Meta Cloud API channel in `internal/notify/whatsapp.go` (stdlib, no SDK). Fires on order-confirmed/shipped/refund alongside email, using `order.ShippingAddress.Phone` (NOT user.Phone — empty for Google-only accounts) normalized to +91 E.164. Mock (log-only) when `WHATSAPP_ACCESS_TOKEN`/`WHATSAPP_PHONE_NUMBER_ID` unset. Template names/language via `WHATSAPP_TPL_*` env — approved Meta templates must match the param order in whatsapp.go.
> - **v3.4 Razorpay primary**: default gateway flipped stripe->razorpay (Stripe India is invite-only; code kept intact). Verify hardening per current Razorpay docs: timing-safe hmac.Equal, order_id signed from DB record (not client), fail-closed in production when RAZORPAY_KEY_SECRET unset. Order creation uses capture:"automatic" + notes + 40-char receipt cap. Webhook handler was already spec-compliant (raw-body HMAC, fail-closed). Frontend: checkout.js from CDN in index.html, handler-function flow (Option B).
> - **Checkout is now transactional** on a replica set (Mongo multi-doc transaction); auto-detects standalone Mongo at startup and falls back to manual compensating rollback.
> - **Product search uses a `$text` index** with weighted relevance ranking (was an un-indexable `$regex` scan).
> - **Wishlist enrichment** is a single `$in` query (was N+1).

---

## 1. What This Service Is

`ecom-core-service` is a **monolithic Go backend for a B2C e-commerce store** (India-focused: paise currency, INR, MSG91 SMS, Razorpay/Cashfree payments, Shiprocket logistics). It exposes a single REST API (`/api/v1`) that powers a storefront and an admin panel.

- **Language/Runtime:** Go 1.22 (toolchain pinned to go1.26.2 in [go.mod](go.mod))
- **HTTP framework:** [Gin](https://github.com/gin-gonic/gin) v1.9.1
- **Datastore:** MongoDB (official `go.mongodb.org/mongo-driver` v1.14)
- **Auth:** Phone + OTP → JWT (HS256), 72h expiry
- **Deploy target:** Docker → Railway (single replica), logs shipped to Grafana/Loki as JSON
- **Entry point:** [cmd/api/main.go](cmd/api/main.go)

It is a **single deployable binary**. There is no separate worker, scheduler, or message queue. A separate "notification microservice" is referenced over HTTP ([pkg/utils/notifier.go](pkg/utils/notifier.go)) but is optional/fire-and-forget.

---

## 2. How to Build, Run, and Develop

```bash
# Run locally (needs MongoDB at localhost:27017 by default)
go run ./cmd/api

# Build the binary
go build -o server ./cmd/api

# Vet / format before committing
go vet ./...
gofmt -l .

# Docker (mirrors production build)
docker build -t ecom-core-service .
docker run -p 8080:8080 --env-file .env ecom-core-service
```

- Config is **100% environment variables** via [internal/config/config.go](internal/config/config.go). Copy [.env.example](.env.example) → `.env`. Every var has a dev-safe fallback, so the service boots with zero config against a local Mongo.
- In `development`, OTP is hard-coded to `123456`, SMS/payments/uploads run in **mock mode**, and the OTP is echoed in the API response.
- There is a committed `api` binary (17MB) in the repo root — it is gitignored going forward but was checked in historically. Do not rely on it; rebuild from source.

> **Note:** There are currently **no tests** in the repo (`*_test.go` count: 0). `go test ./...` is a no-op. Adding tests is a high-value, low-risk contribution — start with [internal/order/handler.go](internal/order/handler.go) (`Create`) and [internal/payment/handler.go](internal/payment/handler.go) (signature verification).

---

## 3. Architecture & Code Layout

```
cmd/api/main.go          # Wiring: config, Mongo connect, index creation, routing, graceful shutdown.
                         # Also contains 3 INLINE handlers (anti-pattern, see §6): /admin/users,
                         # /admin/stats, /admin/analytics/revenue, and the shipping webhook.
internal/
  config/                # Env → Config struct. Single source of config truth.
  middleware/            # JWT auth, admin gate, in-memory IP rate limiter, token generation.
  models/                # ALL domain structs in one file (models.go). bson + json + binding tags.
  <domain>/handler.go    # One package per domain. Each has: Handler{db,(cfg)}, NewHandler(), methods.
pkg/
  logger/                # Custom structured logger (text for dev, JSON for Loki). NOT slog.
  errcodes/              # Central catalog of typed error codes (EAUTH001, EORD004, ...).
  utils/notifier.go      # SMS via MSG91 + fire-and-forget notification-service calls.
  cache/                 # Optional Redis cache. Nil-safe: no-ops if Redis is down/unset,
                         # so callers always need a source-of-truth fallback. Never the SoR.
```

**Domains** (each is `internal/<name>/handler.go`): `auth`, `product` (+categories), `cart`, `order`, `payment`, `shipping`, `coupon`, `wishlist`, `review`, `user`, `upload`.

### The standard handler pattern (follow this for new code)

```go
package widget

var log = logger.New("WIDGET", "SUB")          // package-level structured logger
var _ = errcodes.EWidgetX                       // keep errcodes import alive if only used in some methods

type Handler struct{ db *mongo.Database }       // add cfg *config.Config only if needed
func NewHandler(db *mongo.Database) *Handler { return &Handler{db: db} }

func (h *Handler) DoThing(c *gin.Context) {
    userID := c.GetString("user_id")            // set by AuthMiddleware
    var req struct{ ... `binding:"required"` }
    if err := c.ShouldBindJSON(&req); err != nil { /* 400 + errcode */ return }

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()                              // EVERY db call gets a timeout context

    // ... mongo ops via h.db.Collection("...") ...
    c.JSON(http.StatusOK, gin.H{...})
}
```

### Routing structure ([cmd/api/main.go](cmd/api/main.go))
Three groups under `/api/v1`:
1. **Public** — auth, product/category reads, public review reads, payment & shipping webhooks.
2. **Authenticated** (`AuthMiddleware`) — cart, orders, payments, wishlist, reviews, user profile/addresses.
3. **Admin** (`AuthMiddleware` + `AdminMiddleware`) — product/category/coupon CRUD, order management, refunds, uploads, stats.

### Cross-cutting conventions
- **Money is always `int` paise** (₹1 = 100). Never use floats for money internally; convert only at gateway boundaries (Cashfree/Shiprocket want rupees).
- **IDs are app-generated UUID strings** stored in Mongo `_id` (not ObjectIDs). `bson:"_id,omitempty"`.
- **Soft deletes** everywhere via `is_active: false` (products, categories, coupons). No hard deletes except reviews.
- **Indexes are code-managed** in `ensureIndexes()` in [main.go](cmd/api/main.go) — add new indexes there, not via migrations. OTPs use a TTL index for auto-cleanup.
- **List endpoints** return `{ "<plural>": [...], "total": N }` and normalize nil slices to `[]` so the API never returns `null`.
- **Logging:** use the package `log` (`logger.New(...)`). Use `WarnWithCode`/`ErrorWithCode` with an `errcodes.*` code for anything a client sees. Keys are `snake_case` (e.g. `user_id`, `order_id`).
- **Errors to clients:** `gin.H{"error": "...", "code": errcodes.EXxx.Code}`. The `errcodes` catalog is the contract — reuse existing codes; add new ones to [pkg/errcodes/codes.go](pkg/errcodes/codes.go) and register them in `init()`.

---

## 4. Key Business Logic Worth Knowing

- **Order creation** ([internal/order/handler.go](internal/order/handler.go) `Create` → `placeOrder`): decrements stock per item with `FindOneAndUpdate` guarded by `stock >= qty`, applies the coupon, inserts the order, and clears the cart. On a **replica set this runs in a multi-document transaction** (`placeOrderTxn`); on **standalone Mongo it falls back** (`placeOrderManual`) to manual compensating rollback. Transaction support is detected once at startup (`supportsTransactions`). Shared write logic lives in `buildOrder`. Shipping is free over ₹500 (`subtotal < 50000` → ₹50 fee).
- **Payments** ([internal/payment/handler.go](internal/payment/handler.go)): dual-gateway. `activeGateway()` picks Razorpay or Cashfree by config + credential presence, falling back to `mock`. Razorpay verify uses **HMAC-SHA256 signature check**; Cashfree verify does a **server-side order-status GET**. `markPaymentPaid()` flips both the `payments` and `orders` docs to paid/confirmed.
- **Reviews** mark `is_verified` when the user has a delivered order containing the product; one review per user/product (enforced by a unique compound index + an app check).
- **Cancellation** restores stock and decrements coupon usage; if the order was paid it goes to `refund_pending`.
- **Pincode serviceability / EDD** ([internal/shipping/serviceability.go](internal/shipping/serviceability.go)): `GET /shipping/serviceability/:pincode` (public, hit on the PDP). Read path is **cache → Shiprocket courier-serviceability API → deterministic mock fallback**, with the external call kept off the hot path via a 12h Redis cache keyed `serviceability:{warehouse}:{pincode}`. Returns serviceable/COD/estimated-days and a computed EDD (Sundays skipped). Origin is `WAREHOUSE_PINCODE`. This is the reference example for how to add a cached, 3P-backed read endpoint here.

---

## 5. Are We Built for Scale? — Honest Assessment

**Short answer: it's a solid, well-organized MVP/early-stage monolith. It is NOT yet horizontally scalable, and several pieces will break or degrade under real traffic.** Today it's pinned to `numReplicas = 1` ([railway.toml](railway.toml)), which masks the worst issues.

### Things done right (scale-friendly)
- Stateless request handling + JWT (no server-side sessions to replicate).
- Per-request context timeouts on every DB call — prevents goroutine pileups.
- Sensible MongoDB indexes defined up front, including compound and TTL indexes.
- Pagination on the heavy list endpoints (products, admin orders, reviews).
- Atomic stock decrement via `FindOneAndUpdate` (avoids the classic read-modify-write oversell on a single item).
- Graceful shutdown with `srv.Shutdown`.
- Fire-and-forget SMS via goroutines so notifications don't block the request path.

### Things that will NOT scale (ranked by impact)

1. ~~**In-memory rate limiter** keyed by IP in a per-process `map`.~~ **FIXED** — `RateLimitMiddleware` now uses a Redis-backed fixed-window counter (`Cache.RateAllow`) shared across all replicas, and falls back to the per-process token bucket only when Redis is unavailable. Multi-replica deployments now enforce a consistent global limit.

2. ~~**Analytics loads the entire `orders` collection into memory** twice.~~ **FIXED** — `revenueAnalytics` is now a single `$facet` aggregation and `/admin/stats` uses `$group`. No collection load, no bubble sort. If you extend the dashboard, add facets to the pipeline rather than looping in Go.

3. ~~**No multi-document transactions** for checkout.~~ **FIXED** — checkout runs in a Mongo transaction on a replica set (`placeOrderTxn`), with automatic fallback to manual compensating rollback on standalone Mongo (`placeOrderManual`). **Note:** local dev is usually standalone, so you exercise the fallback path locally — use a replica set (or Atlas) to exercise the transactional path.

4. ~~**Regex `$regex` product search.**~~ **FIXED** — `List` now uses a weighted `$text` index (`product_text`: name×10, tags×5, description×1) with relevance-score sorting when searching. Defined in `ensureIndexes()`.

5. **Caching layer** ([pkg/cache](pkg/cache/cache.go)) now backs serviceability, **product listings** (60s TTL, keyed by query string, busted on any product write), **categories** (5m TTL, busted on category write), and rate limiting. **Still to do (optional):** single-product `GET /products/:id` caching if PDP read volume warrants it.

6. ~~**Wishlist `Get` does N+1 queries.**~~ **FIXED** — enrichment is now a single `$in` query into a map.

7. ~~**Webhooks are not idempotent and partly unverified.**~~ **FIXED** — payment & shipping webhooks now verify their gateway signature/token and payment transitions are idempotent (see §6 and the changelog at the top).

### Verdict
Good bones, clean separation. The security gaps, the analytics blow-up, the multi-replica rate-limit blocker, transactional checkout, search, and the N+1/caching items are all now resolved — the service is in good shape to scale horizontally behind Redis + a Mongo replica set. **Remaining higher-effort items:** zero automated tests (highest priority now), the still-stubbed S3 upload path, ignored Mongo write errors in a few spots, and inline `/admin/*` handlers that should move into a domain package.

---

## 6. Known Issues, Anti-Patterns & Security Gaps

When touching these areas, prefer fixing the root cause over matching the existing pattern.

### Security
- ~~**Payment webhooks do NOT verify signatures.**~~ **FIXED** — `Webhook` reads the raw body once and verifies the Razorpay (`X-Razorpay-Signature`, hex HMAC) / Cashfree (`x-webhook-signature`, base64 HMAC over `timestamp+body`) signature before any state change, using constant-time compare. Fails closed in production if the secret is unset.
- ~~**Shipping webhook is an unauthenticated inline closure.**~~ **FIXED** — moved to `shippingH.Webhook` in [internal/shipping/handler.go](internal/shipping/handler.go), authenticated via `x-api-key` (`SHIPROCKET_WEBHOOK_TOKEN`, constant-time compare), fail-closed in prod, only updates an existing shipment by AWB.
- ~~**Default `JWT_SECRET=dev-secret-key`.**~~ **FIXED** — [main.go](cmd/api/main.go) hard-fails at startup if `JWT_SECRET` is empty or the default while `ENVIRONMENT=production`.
- **CORS defaults to `*`** when unset — now **warns** at startup in production (the code already disables credentials with wildcard). Set explicit origins in prod.
- **OTP is returned in the API response** in development mode and `rand.Intn` (math/rand, not crypto/rand) is used to generate it. Acceptable for dev-mock; ensure prod path uses MSG91 and never echoes codes. **Still open (low risk while dev-only).**

### Correctness
- ~~**Webhooks aren't idempotent.**~~ **FIXED** — `markPaymentPaid` updates the order only while `payment_status == "pending"` and returns `firstTransition`, so retries/replays are safe and never regress an advanced order; confirmation SMS fires once.
- **Many Mongo errors are ignored** (`_, _ =` or unchecked `Decode`). E.g. inline `/admin/users` ignores the cursor error; cart `Get` swallows insert errors. Fine-ish for reads, risky for writes. **Still open.**
- **`fmt.Sscanf` for query-param parsing** silently leaves defaults on bad input — generally safe but means `?page=abc` → page 1 with no error.
- **Order number generation** (`ORD-<unix%100000><rand4>`) can collide; it relies on a unique index to catch dupes but the insert error isn't specifically handled as a retry.

### Style / structure
- **Inline handlers in main.go** (`/admin/users`, `/admin/stats`, `/admin/analytics/revenue`). These belong in their domain packages (`user`, a new `admin`/`analytics`). Moving them is a clean refactor. (The shipping webhook was already moved out.)
- **`var _ = errcodes.EXxx`** lines exist to keep the import alive in packages that don't reference a code in every build. Harmless but a smell — once every code is actually used these can go.
- **Upload to S3 is mocked/stubbed** ([upload/handler.go](internal/upload/handler.go)) — `presigned-url` returns a fake URL and `image` saves to `/tmp` unless `S3_BUCKET` is set, and even then the "real" path is a placeholder (no AWS SDK is imported). Treat uploads as not-yet-implemented for prod.
- **Shiprocket integration is partial** — `createShiprocketShipment` posts an order but doesn't parse AWB/tracking URL back; mock mode is the working path.
- **Two different `slugify` implementations** exist (one in `product`, one in `upload`). Consider consolidating into `pkg/utils`.

---

## 7. Conventions for New Work (do this)

- **New endpoint?** Add the method to the relevant `internal/<domain>/handler.go`, register the route in the correct group in [main.go](cmd/api/main.go), and reuse/extend [errcodes](pkg/errcodes/codes.go).
- **New collection?** Add its struct to [models/models.go](internal/models/models.go) and its indexes to `ensureIndexes()`.
- **Money:** integer paise, always.
- **DB calls:** always `context.WithTimeout`; always `defer cursor.Close(ctx)`; normalize nil slices to `[]` before returning.
- **Logging:** structured `log.Info/Warn/Error(operation, msg, k, v, ...)` with `snake_case` keys; use `*WithCode` variants for client-facing failures.
- **Don't** introduce floats for currency, hard deletes, server-side session state, or per-process mutable caches (they break multi-replica).
- **Prefer** MongoDB aggregation over loading collections into Go memory.
- Run `go vet ./...` and `gofmt` before committing. Match the existing compact brace style (single-line `if x { return }` is used heavily — keep it consistent within a file).

---

## 8. Reference Docs in This Repo
- [README.md](README.md) — quick start, full API reference, and a Done / TODO checklist (kept reasonably current).
- [BACKEND_ARCHITECTURE.md](BACKEND_ARCHITECTURE.md) — deeper architecture write-up (42KB).
- [DATABASE_DESIGN.md](DATABASE_DESIGN.md) — full schema documentation: every collection, field table, indexes with rationale, cross-cutting patterns, scaling posture, and ER diagram.
- [.env.example](.env.example) — every config var with provenance notes.
