# 🛒 E-Commerce Core Service

A complete, production-ready e-commerce backend in Go/Gin/MongoDB. Powers the entire e-commerce platform with Product Catalog, Cart, Orders, Payments (Razorpay), Shipping (Shiprocket), Auth (OTP/JWT), Reviews, Wishlist, Coupons, and Admin APIs.

## 🚀 Quick Start

```bash
cd ecom-core-service
go mod tidy
MONGO_URI="mongodb://localhost:27017/ecom" go run ./cmd/api/
```

Server starts on **http://localhost:8080**

## ✅ What's Done

| Feature | Status | Details |
|---------|--------|---------|
| Auth (OTP + JWT) | ✅ Complete | Phone OTP login, mock mode for dev, MSG91 for prod |
| Product Catalog | ✅ Complete | CRUD, search, categories, variants, slugs, images |
| Shopping Cart | ✅ Complete | Add/remove/update, stock validation, auto-pricing |
| Orders | ✅ Complete | Create from cart, coupons, stock deduction, order numbers |
| Payments (Razorpay) | ✅ Complete | Real Razorpay REST API, signature verification, webhooks |
| Shipping (Shiprocket) | ✅ Complete | AWB creation, tracking, webhooks, mock mode |
| Coupons | ✅ Complete | Percentage/fixed, min order, max discount, expiry, usage limit |
| Wishlist | ✅ Complete | Add/remove/list |
| Reviews | ✅ Complete | Star rating, text review, admin moderation |
| User Profile | ✅ Complete | View/update profile, addresses |
| Image Upload | ✅ Complete | S3 upload with local fallback |
| Admin APIs | ✅ Complete | Products CRUD, orders, users, stats dashboard |
| SMS Notifications | ✅ Complete | MSG91 direct SMS for order updates (confirmation, shipped, delivered, cancelled) |
| Structured Logging | ✅ Complete | Color-coded logs with 60+ error codes |
| Rate Limiting | ✅ Complete | Per-IP rate limiting on auth routes |

## ❌ What's Left To Do

### 🔴 Must Have (Before Production)
- [ ] **Razorpay Test Keys** — Sign up at [dashboard.razorpay.com](https://dashboard.razorpay.com), get test keys, set `RAZORPAY_KEY_ID` + `RAZORPAY_KEY_SECRET`
- [ ] **MongoDB Atlas** — Create free M0 cluster at [cloud.mongodb.com](https://cloud.mongodb.com), update `MONGO_URI`
- [ ] **MSG91 Account** — Sign up at [msg91.com](https://msg91.com), get auth key, set `OTP_SERVICE=msg91`, `MSG91_AUTH_KEY`, `MSG91_TEMPLATE_ID`
- [ ] **JWT Secret** — Change `JWT_SECRET` from default `dev-secret-key` to a strong random string
- [ ] **CORS Configuration** — Update allowed origins for production domain

### 🟡 Should Have (Enhancement)
- [ ] **Real Shiprocket Integration** — Sign up at [app.shiprocket.in](https://app.shiprocket.in), set credentials (currently mock)
- [ ] **S3 Image Upload** — Configure `S3_BUCKET` + `S3_REGION` + AWS credentials (currently saves locally)
- [ ] **Email Notifications** — Add email templates for order confirmation, shipping updates
- [ ] **Search Enhancement** — Add MongoDB text indexes for full-text product search
- [ ] **Pagination Metadata** — Add `next_page`, `has_more` to list endpoints
- [ ] **Order Invoice PDF** — Generate downloadable invoice for each order
- [ ] **Admin Analytics** — Revenue charts, top products, conversion rates

### 🔵 Nice To Have (Future)
- [ ] **Redis Caching** — Cache product lists, categories for faster reads
- [ ] **Elasticsearch** — For advanced product search with filters, facets
- [ ] **Multi-tenant Support** — Support multiple stores from one backend
- [ ] **Webhook Signature Verification** — Verify Razorpay webhook signatures
- [ ] **Database Migrations** — Version-controlled schema changes
- [ ] **Unit Tests** — Test coverage for handlers
- [ ] **Docker Compose** — One-command setup with MongoDB + Redis + Backend
- [ ] **API Documentation** — Swagger/OpenAPI spec generation
- [ ] **Inventory Alerts** — Low stock notifications to admin

## 📡 Complete API Reference

### Auth (Public)
| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/v1/auth/send-otp` | Send OTP to phone |
| POST | `/api/v1/auth/verify-otp` | Verify OTP, get JWT |

### Products (Public)
| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/v1/products` | List products (`?category=&search=&page=&limit=`) |
| GET | `/api/v1/products/:id` | Get product by ID or slug |
| GET | `/api/v1/categories` | List categories |

### Cart (Auth Required)
| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/v1/cart` | Get user's cart |
| POST | `/api/v1/cart/add` | Add item to cart |
| PUT | `/api/v1/cart/item/:productId` | Update quantity |
| DELETE | `/api/v1/cart/item/:productId` | Remove item |
| DELETE | `/api/v1/cart` | Clear cart |

### Orders (Auth Required)
| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/v1/orders` | Create order from cart |
| GET | `/api/v1/orders` | List user's orders |
| GET | `/api/v1/orders/:id` | Get order details |
| POST | `/api/v1/orders/:id/cancel` | Cancel order |

### Payments (Auth Required)
| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/v1/payment/create` | Create Razorpay order (real API call) |
| POST | `/api/v1/payment/verify` | Verify payment signature |
| POST | `/api/v1/payment/webhook` | Razorpay webhook (public) |

### Wishlist (Auth Required)
| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/v1/wishlist` | Get wishlist |
| POST | `/api/v1/wishlist/:productId` | Add to wishlist |
| DELETE | `/api/v1/wishlist/:productId` | Remove from wishlist |

### Reviews (Auth Required)
| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/v1/reviews` | Submit a review |
| GET | `/api/v1/reviews/:productId` | Get product reviews |

### Shipping
| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/v1/shipping/track/:orderId` | Track shipment |

### Admin (Admin Only)
| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/v1/admin/products` | Create product |
| PUT | `/api/v1/admin/products/:id` | Update product |
| DELETE | `/api/v1/admin/products/:id` | Delete product |
| POST | `/api/v1/admin/categories` | Create category |
| GET | `/api/v1/admin/orders` | List all orders |
| PUT | `/api/v1/admin/order/:id/status` | Update order status + send SMS |
| POST | `/api/v1/admin/order/:id/refund` | Process refund |
| POST | `/api/v1/admin/shipping/create` | Create shipment |
| GET | `/api/v1/admin/users` | List all users |
| GET | `/api/v1/admin/stats` | Dashboard stats |
| POST | `/api/v1/admin/upload` | Upload image (S3/local) |

## 🏗 Architecture

```
ecom-core-service/
├── cmd/api/main.go              # Entry point, routes
├── internal/
│   ├── auth/handler.go          # OTP + JWT authentication
│   ├── product/handler.go       # CRUD, search, categories
│   ├── cart/handler.go          # Cart operations
│   ├── order/handler.go         # Orders + SMS notifications
│   ├── payment/handler.go       # Razorpay REST API integration
│   ├── shipping/handler.go      # Shiprocket integration
│   ├── coupon/handler.go        # Coupon management
│   ├── wishlist/handler.go      # Wishlist CRUD
│   ├── review/handler.go        # Product reviews
│   ├── user/handler.go          # Profile management
│   ├── upload/handler.go        # S3/local file upload
│   ├── config/config.go         # Environment configuration
│   ├── middleware/middleware.go  # Auth, CORS, rate limiting
│   └── models/models.go         # All data models
├── pkg/
│   ├── logger/logger.go         # Structured color-coded logging
│   ├── errcodes/codes.go        # 60+ categorized error codes
│   └── utils/notifier.go        # MSG91 SMS + notification service
└── go.mod
```

## ⚙️ Configuration

| Variable | Default | Required | Description |
|----------|---------|----------|-------------|
| PORT | 8080 | No | Server port |
| MONGO_URI | mongodb://localhost:27017/ecom | **Yes** | MongoDB connection |
| MONGO_DB | ecom | No | Database name |
| JWT_SECRET | dev-secret-key | **Yes (prod)** | JWT signing key |
| OTP_SERVICE | mock | No | `mock` or `msg91` |
| MSG91_AUTH_KEY | — | For SMS | MSG91 auth key |
| MSG91_TEMPLATE_ID | — | For OTP | MSG91 OTP template |
| SMS_MODE | mock | No | `mock` or `msg91` for order SMS |
| RAZORPAY_KEY_ID | — | For payments | Razorpay key ID |
| RAZORPAY_KEY_SECRET | — | For payments | Razorpay secret |
| SHIPROCKET_EMAIL | — | For shipping | Shiprocket email |
| SHIPROCKET_PASSWORD | — | For shipping | Shiprocket password |
| S3_BUCKET | — | For uploads | AWS S3 bucket name |
| S3_REGION | ap-south-1 | No | AWS region |
| NOTIFICATION_SERVICE_URL | http://localhost:9090 | No | Notification service URL |

## 📄 License
MIT
