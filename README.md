# 🛒 LucubraElec — E-Commerce Core Service

Go/Gin/MongoDB/Redis backend powering [LucubraElec.in](https://lucubraelec.in) — electronics & components store. Product catalog, cart, orders, payments (Razorpay), shipping (Shipmozo), Google OAuth + guest checkout, reviews, wishlist, coupons, invoicing, and admin APIs.

📄 **[Database Design Documentation](DATABASE_DESIGN.md)** — full schema, indexes, scaling rationale, and ER diagram.

## 🚀 Quick Start

```bash
go mod tidy
set -a; source .env; set +a   # no godotenv — env must be in the process
go run ./cmd/api/
```

Server starts on **http://localhost:8080**. Health: `GET /health`.

## ✅ Feature Status

| Feature | Status | Details |
|---------|--------|---------|
| Auth | ✅ | Google OAuth (JWT sessions); guest checkout needs no account |
| Guest checkout | ✅ | Public `/guest/*` routes, stateless HMAC token (orderID\|phone), rate-limited, track by order number + phone |
| Products | ✅ | CRUD, search, 3-level categories, variants, unique slugs, images |
| Cart | ✅ | Total-quantity stock validation, live stock enrichment, guest cart merge |
| Orders | ✅ | Atomic stock reservation, status state machine, order numbers (ORD-XXXXXXXXX) |
| Payments | ✅ | Razorpay (live test-mode verified): signature verification (timing-safe, fail-closed), webhooks, partial payment (whole-rupee advance + COD remainder), refunds |
| All-or-nothing flow | ✅ | Abandon on modal dismiss releases stock+coupon; late-UPI webhook resurrection with auto-refund fallback; 24h purge |
| Expiry sweeper | ✅ | Unpaid orders auto-expire after 30 min (stock + coupon released) |
| Shipping | ✅ | Shipmozo (live): push-order + auto-assign → AWB, rate-calculator-backed serviceability, tracking, delivery webhook (sets `cod_collected`), mock provider when keys unset |
| Notifications | ✅ | WhatsApp via Meta Cloud API (mock until credentials set), guest-aware (uses shipping phone); email skips gracefully |
| Coupons | ✅ | %/fixed, min order, max discount, expiry, usage limits, whole-rupee rounding |
| Invoices | ✅ | Per-order invoice endpoint |
| Admin APIs | ✅ | Products/orders/users/stats/refunds, order search (all ID shapes), status transitions enforced server-side |
| Observability | ✅ | Structured logging (60+ error codes), per-IP rate limiting, request IDs |
| Tests | ✅ | Unit tests: payments, orders, guest tokens, shipping adapter, state machine |

## ❌ Left for launch

- [ ] Deploy: Railway (backend) + MongoDB Atlas + Upstash Redis
- [ ] Register live webhooks: Razorpay + Shipmozo → `https://api.lucubraelec.in/...`
- [ ] Razorpay live-mode keys (KYC pending; test-mode works now)
- [ ] Meta/WhatsApp Business credentials (notifications currently mock/logged)
- [ ] Shipmozo dashboard: auto-assign rule + wallet recharge

## 📡 API Highlights

Public: `GET /products`, `GET /products/:idOrSlug`, `GET /categories/tree`, `GET /shipping/serviceability/:pincode`, `POST /coupons/validate`, `POST /payment/webhook`, `POST /shipping/webhook`
Guest: `POST /guest/orders`, `POST /guest/payment/create|verify`, `POST /guest/orders/:id/abandon`, `GET /guest/track`
Authed: cart CRUD, `POST /orders`, `POST /orders/:id/abandon`, wishlist, reviews, profile
Admin: products/categories/coupons CRUD, `GET /admin/orders?q=&status=&payment_status=`, `PUT /admin/orders/:id/status` (state-machine enforced), `POST /admin/orders/:id/ship|refund`, stats, returns, invoices

## ⚙️ Key Configuration

| Variable | Required | Description |
|----------|----------|-------------|
| MONGO_URI / MONGO_DB | Yes | MongoDB connection |
| REDIS_URL | Yes | Cache + serviceability cache |
| JWT_SECRET | Yes (prod) | JWT signing key |
| GOOGLE_CLIENT_ID | Yes (prod) | Google OAuth |
| GUEST_ORDER_SECRET | Yes (prod) | HMAC key for guest tokens (fail-closed) |
| PAYMENT_GATEWAY | No | `razorpay` (default; `stripe` parked) |
| RAZORPAY_KEY_ID / KEY_SECRET / WEBHOOK_SECRET | For payments | Signature verification fail-closed in prod |
| SHIPMOZO_PUBLIC_KEY / PRIVATE_KEY | For shipping | Absent → mock provider |
| SHIPMOZO_WAREHOUSE_ID | For shipping | Registered pickup warehouse (required for AWB assign) |
| WAREHOUSE_PINCODE | For shipping | Origin pincode for serviceability |
| WHATSAPP_* (Meta Cloud API) | For notifications | Absent → mock mode (logs) |
| PENDING_ORDER_TTL_MINUTES | No | Unpaid-order expiry (default 30) |
| LOG_LEVEL | No | `info` default; `debug` logs raw courier API bodies |

## 📄 License
MIT
