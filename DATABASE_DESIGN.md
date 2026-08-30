# Database Design — ecom-core-service

> **See also `DB_SCHEMA_ERD.md`** — the complete structural reference: full Mermaid
> ER diagram with cardinalities, every collection's field tables, relationship
> enforcement matrix, and documented schema quirks (audited 2026-08-01).

## Entity-Relationship Diagram

```
┌──────────┐       ┌──────────────┐       ┌───────────┐
│  users   │──1:N──│   orders     │──1:1──│ payments  │
│          │       │              │──1:1──│ shipments │
│ [addrs]  │       │ [items,addr] │       └───────────┘
└──────────┘       └──────────────┘
     │                    │
     │ 1:1                │ N:1
     ▼                    ▼
┌──────────┐       ┌──────────────┐       ┌───────────┐
│  carts   │       │  products    │──N:1──│categories │
│ [items]  │       │ [variants]   │       │(hierarchy)│
└──────────┘       └──────────────┘       └───────────┘
     │                    │                      │
     │                    │ 1:N                  │ self-ref
     │                    ▼                      │ parent_id
     │             ┌──────────────┐              │ path
     │             │   reviews    │              ▼
     │             └──────────────┘       ┌───────────┐
     │                                    │  (tree)   │
     │ user_id                            └───────────┘
     ▼
┌──────────┐       ┌──────────────┐
│ wishlists│       │   coupons    │
│ [items]  │       └──────────────┘
└──────────┘
     │
     │             ┌──────────────┐       ┌───────────┐
     │             │    otps      │       │  admins   │
     │             │  (TTL auto)  │       └───────────┘
     │             └──────────────┘
```

Arrows show app-level UUID references (no DB-level foreign keys or joins).

---

## Collections

### users

**Purpose:** Store registered customers (phone-OTP or Google sign-in). Addresses embedded for read locality on checkout.

| Field | Type | Notes |
|-------|------|-------|
| _id | string (UUID) | App-generated |
| phone | string | E.164 format, unique (partial: non-empty only) |
| email | string | Optional, sparse index |
| name | string | Display name |
| avatar | string | URL |
| google_sub | string | Google subject ID for OAuth link, sparse |
| is_admin | bool | Legacy flag (admin auth now separate) |
| addresses | []Address | Embedded array, max 10 |
| created_at | time.Time | |
| updated_at | time.Time | |

**Indexes:**
| Index | Type | Rationale |
|-------|------|-----------|
| phone (partial: phone > "") | unique | Login lookup; partial to allow multiple Google-only accounts with empty phone |
| email | sparse | Optional email lookup |
| google_sub | sparse | Google sign-in lookup |

---

### admins

**Purpose:** Dedicated admin accounts with bcrypt passwords, separate from customer auth.

| Field | Type | Notes |
|-------|------|-------|
| _id | string (UUID) | App-generated |
| email | string | Unique login identifier |
| name | string | Display name |
| password_hash | string | bcrypt hash |
| created_at | time.Time | |
| updated_at | time.Time | |

**Indexes:**
| Index | Type | Rationale |
|-------|------|-----------|
| email | unique | Login lookup, prevent duplicates |

---

### otps

**Purpose:** Temporary OTP records for phone verification. Auto-cleaned by TTL index.

| Field | Type | Notes |
|-------|------|-------|
| _id | string (UUID) | |
| phone | string | Target phone number |
| code | string | 6-digit OTP |
| expires_at | time.Time | TTL expiry timestamp |
| verified | bool | Prevents reuse |
| created_at | time.Time | |

**Indexes:**
| Index | Type | Rationale |
|-------|------|-----------|
| phone | standard | Lookup by phone during verify |
| expires_at | TTL (0s) | MongoDB auto-deletes expired docs |

---

### products

**Purpose:** Product catalog. Central entity referenced by orders, carts, wishlists, reviews.

| Field | Type | Notes |
|-------|------|-------|
| _id | string (UUID) | |
| name | string | Display name |
| slug | string | URL-safe identifier, unique |
| description | string | Full text, searchable |
| category | string | Category slug reference |
| price | int | In paise (₹1 = 100) |
| compare_at_price | int | Strikethrough price (paise) |
| images | []string | URLs |
| thumbnail | string | Primary image URL |
| variants | []Variant | Embedded (id, name, value, stock, price override) |
| tags | []string | Searchable tags |
| is_active | bool | Soft-delete flag |
| stock | int | Available quantity |
| sku | string | Auto-generated or manual |
| weight | int | Grams (for shipping calc) |
| advance_percent | *int | 0-100, nil=use global default |
| created_at | time.Time | |
| updated_at | time.Time | |

**Indexes:**
| Index | Type | Rationale |
|-------|------|-----------|
| slug | unique | URL routing, GET by slug |
| category | standard | Filter by category |
| is_active + created_at | compound | Listing sorted by newest |
| sku | standard | Admin lookup |
| price | standard | Price range filters |
| tags | standard | Tag-based filtering |
| name | standard | Prefix regex suggest (anchored `^term`) |
| name + tags + description (text, weighted 10:5:1) | text ("product_text") | Full-text search with relevance ranking |

---

### categories

**Purpose:** Hierarchical product classification (max 3 levels). Uses materialized path pattern for efficient ancestor/descendant queries.

| Field | Type | Notes |
|-------|------|-------|
| _id | string (UUID) | |
| name | string | Display name |
| slug | string | URL-safe, unique |
| parent_id | string | Parent category ID (empty for roots) |
| level | int | 1=root, 2=sub, 3=sub-sub |
| path | string | Materialized ancestor path, e.g. "rootID/subID" |
| image | string | Category image URL |
| is_active | bool | Soft-delete flag |
| sort_order | int | Display ordering |
| created_at | time.Time | |
| updated_at | time.Time | |

**Indexes:**
| Index | Type | Rationale |
|-------|------|-----------|
| slug | unique | URL routing |
| is_active | standard | Active-only listings |
| parent_id | standard | Find children of a category |
| path | standard | Descendant queries via prefix regex |
| is_active + sort_order | compound | Sorted active listings |

---

### carts

**Purpose:** Per-user shopping cart. One cart per user (upsert pattern).

| Field | Type | Notes |
|-------|------|-------|
| _id | string (UUID) | |
| user_id | string | Owner, unique |
| items | []CartItem | Embedded (product_id, variant_id, name, price, quantity, image) |
| created_at | time.Time | |
| updated_at | time.Time | |

**Indexes:**
| Index | Type | Rationale |
|-------|------|-----------|
| user_id | unique | One cart per user, fast lookup |

---

### orders

**Purpose:** Placed orders. Immutable line items snapshotted from cart at checkout time.

| Field | Type | Notes |
|-------|------|-------|
| _id | string (UUID) | |
| user_id | string | Customer |
| order_number | string | Human-readable (ORD-XXXXX-YYYY) |
| items | []OrderItem | Snapshotted line items |
| subtotal | int | Paise |
| shipping_cost | int | Paise (free > ₹500) |
| discount | int | Paise |
| total | int | Paise, final amount |
| payment_plan | string | "full" or "partial" |
| advance_amount | int | Online portion (partial plan) |
| cod_amount | int | Cash portion (partial plan) |
| cod_collected | bool | COD collected on delivery |
| coupon_code | string | Applied coupon |
| status | string | pending/confirmed/shipped/delivered/cancelled |
| payment_status | string | pending/paid/refund_pending/refunded |
| payment_method | string | online/cod |
| payment_id | string | Payment doc ref |
| shipping_address | Address | Embedded, snapshotted |
| billing_address | Address | Embedded |
| tracking_id | string | |
| tracking_url | string | |
| shipment_id | string | Shipment doc ref |
| notes | string | Customer notes |
| cancel_reason | string | |
| cancelled_at | *time.Time | |
| refund_id | string | |
| refund_amount | int | Paise |
| refunded_at | *time.Time | |
| created_at | time.Time | |
| updated_at | time.Time | |

**Indexes:**
| Index | Type | Rationale |
|-------|------|-----------|
| user_id + created_at (desc) | compound | User's orders newest-first |
| order_number | unique | Lookup by human ID |
| status | standard | Admin filtering by status |
| payment_status | standard | Payment reconciliation queries |
| created_at (desc) | standard | Admin listing sorted by date |

---

### payments

**Purpose:** Payment intent records linked 1:1 to orders. Supports multi-gateway (Stripe primary, Razorpay/Cashfree available).

| Field | Type | Notes |
|-------|------|-------|
| _id | string (UUID) | |
| order_id | string | Order ref |
| user_id | string | Customer |
| razorpay_order_id | string | Gateway order ID (used across all gateways) |
| razorpay_payment_id | string | Gateway payment ID |
| razorpay_signature | string | Verification signature |
| amount | int | Paise |
| currency | string | "INR" |
| method | string | upi/card/netbanking |
| status | string | pending/paid/failed/refunded |
| created_at | time.Time | |
| updated_at | time.Time | |

**Indexes:**
| Index | Type | Rationale |
|-------|------|-----------|
| order_id | standard | Find payment by order |
| razorpay_order_id | standard | Webhook lookup by gateway ID |

---

### shipments

**Purpose:** Shipping/courier records. One per order (idempotent creation).

| Field | Type | Notes |
|-------|------|-------|
| _id | string (UUID) | |
| order_id | string | Order ref |
| provider | string | "shipmozo" / "mock" |
| shipment_id | string | Provider's shipment ID |
| awb | string | Air waybill number |
| courier_name | string | |
| tracking_url | string | |
| label_url | string | |
| status | string | created/in_transit/delivered/rto |
| events | []TrackingEvent | Status history |
| est_delivery | string | Estimated date |
| weight | int | Grams |
| length, width, height | int | cm |
| created_at | time.Time | |
| updated_at | time.Time | |

**Indexes:**
| Index | Type | Rationale |
|-------|------|-----------|
| order_id | standard | Find shipment by order |
| awb | standard | Webhook lookup by AWB |

---

### coupons

**Purpose:** Discount codes with usage limits, expiry, and min-order thresholds.

| Field | Type | Notes |
|-------|------|-------|
| _id | string (UUID) | |
| code | string | Unique coupon code |
| type | string | "percentage" or "fixed" |
| value | int | Discount amount (paise for fixed, percent for percentage) |
| min_order | int | Minimum cart total (paise) |
| max_discount | int | Cap for percentage coupons (paise) |
| usage_limit | int | Max total uses |
| used_count | int | Current uses |
| is_active | bool | Soft-delete |
| description | string | Admin notes |
| expires_at | time.Time | |
| created_at | time.Time | |
| updated_at | time.Time | |

**Indexes:**
| Index | Type | Rationale |
|-------|------|-----------|
| code | unique | Lookup + prevent duplicates |

---

### wishlists

**Purpose:** Per-user saved-for-later product list.

| Field | Type | Notes |
|-------|------|-------|
| _id | string (UUID) | |
| user_id | string | Owner, unique |
| items | []WishlistItem | Embedded (product_id, added_at) |
| created_at | time.Time | |
| updated_at | time.Time | |

**Indexes:**
| Index | Type | Rationale |
|-------|------|-----------|
| user_id | unique | One wishlist per user |

---

### reviews

**Purpose:** Product reviews with star ratings. One review per user per product.

| Field | Type | Notes |
|-------|------|-------|
| _id | string (UUID) | |
| product_id | string | Product ref |
| user_id | string | Author |
| user_name | string | Denormalized for display |
| user_phone | string | Denormalized |
| rating | int | 1-5 stars |
| title | string | |
| comment | string | Review body |
| images | []string | Review images |
| is_verified | bool | User has delivered order with this product |
| is_approved | bool | Admin moderation flag |
| created_at | time.Time | |
| updated_at | time.Time | |

**Indexes:**
| Index | Type | Rationale |
|-------|------|-----------|
| product_id + created_at (desc) | compound | Product review listing |
| user_id | standard | User's reviews |
| product_id + user_id | unique | One review per user per product |

---

## Cross-Cutting Patterns

### Integer-Paise Money
All monetary values are stored as **int (paise)** — ₹1 = 100 paise. No floating point anywhere in the data layer. Conversion to rupees happens only at gateway boundaries (Stripe/Cashfree APIs expect rupees). This eliminates IEEE 754 rounding errors entirely.

### App-Generated UUID `_id`s
Every document uses a UUID v4 string as `_id` (not MongoDB ObjectIDs). Generated at application layer via `github.com/google/uuid`. This decouples ID generation from MongoDB, enables predictable testing, and avoids ObjectID timestamp-leakage.

### Soft Deletes
Products, categories, and coupons use `is_active: false` for soft deletion. No hard deletes occur on these collections (except reviews which do hard-delete). Queries always filter `is_active: true` for public endpoints.

### Code-Managed Indexes
All indexes are defined in `ensureIndexes()` in `cmd/api/main.go` and applied at every startup. No migration tool or external script needed. Safe to re-run (idempotent via MongoDB's `CreateOne` which no-ops if the index already exists with the same spec).

### Embedded Addresses on Users
User addresses are an embedded array (max 10) rather than a separate collection. **Tradeoff:** eliminates a join for the common checkout read (fetch user + addresses in one doc), but limits address history and makes address sharing across collections require snapshotting. The order's `shipping_address` is snapshotted at checkout time — never a reference — so address edits don't retroactively change order history.

### Category Materialized-Path Hierarchy (Max 3 Levels)

**Pattern:** Each category stores its `level` (1-3), `parent_id`, and a `path` string of ancestor IDs separated by `/` (e.g., `"rootID/subID"`). Root categories have `path: ""` and `level: 1`.

**Rationale vs alternatives:**
| Approach | Descendant query | Insert/Move | Depth enforcement |
|----------|-----------------|-------------|-------------------|
| Adjacency list (parent_id only) | Recursive / N+1 | O(1) | Manual |
| **Materialized path** (chosen) | Single regex prefix query | O(1) + path recompute | Trivial (level field) |
| Nested set (lft/rgt) | Single range query | O(N) rebalance | Manual |

Materialized path wins for this use case: reads dominate, hierarchy is shallow (max 3), and moves/inserts are rare admin operations.

**Constraints enforced at application layer:**
- Max depth = 3 (rejected if parent.Level + 1 > 3)
- No cycles (parent cannot be self or a descendant — checked via path inspection)
- Cannot soft-delete a category with active children (must move/delete children first)

### Transactional Checkout
Order creation runs inside a **MongoDB multi-document transaction** (requires replica set). Detects standalone Mongo at startup and falls back to manual compensating rollback (re-increment stock on failure). This ensures atomic stock-decrement + order-insert + cart-clear.

---

## Scaling Section

### Current Posture

**Caching (Redis):**
| Layer | TTL | Bust trigger |
|-------|-----|-------------|
| Product listings | 60s | Any product write |
| Categories (flat + tree) | 5m | Any category write |
| Search suggestions | 60s | None (short TTL) |
| Pincode serviceability | 12h | None (external data) |
| Rate limiter | Window-based | Auto-expire |

**Search:** Product text search uses MongoDB `$text` index with weighted fields (name×10, tags×5, description×1). Search suggestions use a hybrid prefix-regex + text-search strategy with dedup.

### Growth Path

**MongoDB Replica Set → Sharding:**

| Collection | Shard key candidate | Rationale |
|------------|-------------------|-----------|
| orders | user_id (hashed) | Even distribution, user queries are local |
| products | _unsharded_ | Catalog is typically small enough for a single replica set |
| users | _id (hashed) | Even distribution |
| payments | order_id (hashed) | Collocates with order lookups |
| reviews | product_id (hashed) | Product page locality |

Most collections (carts, wishlists, coupons, categories) are small enough to remain unsharded indefinitely.

**Search Upgrade Path:**

Current: `$text` index + anchored prefix regex → sufficient for catalogs up to ~50-100k SKUs.

When to upgrade: catalog > 100k SKUs, or requirements emerge for fuzzy/typo-tolerant search, faceted filtering, or synonym expansion.

**Migration to Atlas Search or Elasticsearch:**
1. **Sync pipeline:** MongoDB Change Streams → transformation lambda → index into Atlas Search / Elasticsearch. Change streams provide at-least-once delivery with resume tokens for reliability.
2. **Cutover:** Keep the `$text` index as fallback. Route suggest/search queries to the new engine behind a feature flag. Validate result quality with A/B testing on CTR.
3. **Schema in search engine:** Denormalized product documents with category names, variant attributes, and computed fields (discount percentage, popularity score) for ranking.

The current architecture (stateless app, Redis cache, Mongo) scales vertically to ~10k concurrent users on a single replica before needing horizontal scaling (multiple app replicas behind a load balancer, which is already supported by the Redis-backed rate limiter and shared cache).

---

### return_requests

**Purpose:** Track customer return/replacement requests per order.

| Field | Type | Notes |
|-------|------|-------|
| _id | string (UUID) | App-generated |
| order_id | string | Order ref |
| user_id | string | Customer |
| items | []ReturnItem | Embedded (product_id, qty, reason) |
| type | string | "return" or "replacement" |
| status | string | requested / approved / rejected / completed |
| comment | string | Customer comment |
| admin_note | string | Admin internal note |
| created_at | time.Time | |
| updated_at | time.Time | |

**Indexes:**
| Index | Type | Rationale |
|-------|------|-----------|
| order_id | standard | Lookup by order |
| user_id + created_at (desc) | compound | User's requests newest-first |
| status + created_at (desc) | compound | Admin filtering by status |

---

### contact_messages

**Purpose:** Public contact form submissions for customer inquiries.

| Field | Type | Notes |
|-------|------|-------|
| _id | string (UUID) | App-generated |
| name | string | Sender name |
| email | string | Sender email |
| phone | string | Optional phone |
| subject | string | Message subject |
| message | string | Message body |
| status | string | "open" or "resolved" |
| created_at | time.Time | |
| updated_at | time.Time | |

**Indexes:**
| Index | Type | Rationale |
|-------|------|-----------|
| status + created_at (desc) | compound | Admin filtered listing |
| created_at (desc) | standard | Chronological listing |

---

### counters

**Purpose:** Atomic sequence counters for invoice numbering.

| Field | Type | Notes |
|-------|------|-------|
| _id | string | Counter name, e.g. "invoice_2526" |
| seq | int | Current sequence value |

**Indexes:** Uses `_id` (default). Upsert pattern via `findOneAndUpdate($inc)`.
