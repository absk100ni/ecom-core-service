# 🛒 E-Commerce Core Service

A complete, production-ready e-commerce backend in Go. Includes Product Catalog, Cart, Orders, Payments (Razorpay), Shipping (Shiprocket), Auth (OTP/JWT), and Admin APIs. Can power ANY e-commerce business.

## 🚀 Quick Start

```bash
cd ecom-core-service
go mod tidy
MONGO_URI="mongodb://localhost:27017/ecom" go run ./cmd/api/
```

Server starts on **http://localhost:8080**

## 📡 Complete API Reference

### Auth (Public)
| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/v1/auth/send-otp` | Send OTP to phone |
| POST | `/api/v1/auth/verify-otp` | Verify OTP, get JWT |

### Products (Public)
| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/v1/products` | List products (`?category=&search=`) |
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

### Payments (Auth Required)
| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/v1/payment/create` | Create Razorpay order |
| POST | `/api/v1/payment/verify` | Verify payment signature |
| POST | `/api/v1/payment/webhook` | Razorpay webhook (public) |

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
| PUT | `/api/v1/admin/order/:id/status` | Update order status |
| POST | `/api/v1/admin/shipping/create` | Create shipment |
| GET | `/api/v1/admin/users` | List all users |
| GET | `/api/v1/admin/stats` | Dashboard stats |

## 🏗 Architecture

```
ecom-core-service/
├── cmd/api/main.go           # Entry point, routes
├── internal/
│   ├── auth/                 # OTP + JWT authentication
│   ├── product/              # CRUD, search, categories
│   ├── cart/                 # Add, remove, update, clear
│   ├── order/                # Create from cart, coupon, stock deduction
│   ├── payment/              # Razorpay create/verify/webhook
│   ├── shipping/             # Shiprocket integration
│   ├── config/               # Environment configuration
│   ├── middleware/            # Auth, rate limiting
│   └── models/               # All data models
└── go.mod
```

## 🔑 Key Features

- **Product Catalog**: Full CRUD, search by name/tag, categories, variants, images
- **Shopping Cart**: Add/remove/update, auto-fetch product info, stock validation
- **Orders**: Create from cart, coupon support, auto stock deduction, order numbers
- **Payments**: Razorpay integration (create order, verify signature, webhook)
- **Shipping**: Shiprocket API integration, AWB tracking, mock mode for dev
- **Auth**: Phone OTP (mock/MSG91), JWT tokens
- **Admin**: Product management, order management, user list, dashboard stats
- **Coupons**: Percentage/fixed discount, min order, max discount, usage limit, expiry
- **Free Shipping**: Auto-free above ₹500

## ⚙️ Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| PORT | 8080 | Server port |
| MONGO_URI | mongodb://localhost:27017/ecom | MongoDB |
| JWT_SECRET | dev-secret-key | JWT signing key |
| OTP_SERVICE | mock | `mock` or `msg91` |
| MSG91_AUTH_KEY | — | MSG91 auth key |
| RAZORPAY_KEY_ID | — | Razorpay key |
| RAZORPAY_KEY_SECRET | — | Razorpay secret |
| SHIPROCKET_TOKEN | — | Shiprocket API token |
| NOTIFICATION_SERVICE_URL | http://localhost:9090 | Notification service |

## 📄 License
MIT
