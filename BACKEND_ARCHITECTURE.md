# 🏗️ E-Commerce Core Service — Backend Architecture & Design Document

> **Service:** `ecom-core-service`  
> **Stack:** Go (Gin) + MongoDB + Redis  
> **Payments:** Razorpay  
> **Shipping:** Shiprocket  
> **Notifications:** Custom microservice (SMS/Email/Push)  
> **Last Updated:** April 2026

---

## Table of Contents

1. [System Overview](#1-system-overview)
2. [Database Schema (MongoDB)](#2-database-schema-mongodb)
3. [Product Upload Strategy](#3-product-upload-strategy)
4. [Image Upload & Storage Pipeline](#4-image-upload--storage-pipeline)
5. [Admin Product Management Flow](#5-admin-product-management-flow)
6. [Order Creation Flow](#6-order-creation-flow)
7. [Payment Flow (Razorpay)](#7-payment-flow-razorpay)
8. [Pricing Strategy](#8-pricing-strategy)
9. [Inventory Management](#9-inventory-management)
10. [Shipping & Fulfillment](#10-shipping--fulfillment)
11. [API Reference](#11-api-reference)
12. [Architecture Decisions](#12-architecture-decisions)
13. [What's Missing & Roadmap](#13-whats-missing--roadmap)

---

## 1. System Overview

```
┌─────────────┐     ┌──────────────┐     ┌────────────────────┐
│  ecom-store  │────▶│              │────▶│      MongoDB       │
│  (Customer)  │     │  ecom-core-  │     │  ┌──────────────┐  │
├─────────────┤     │   service    │     │  │  products    │  │
│ admin-panel  │────▶│  (Go/Gin)   │     │  │  categories  │  │
│  (Admin UI)  │     │  Port 8080  │     │  │  users       │  │
└─────────────┘     │              │     │  │  carts       │  │
                    │              │     │  │  orders      │  │
                    │              │────▶│  │  payments    │  │
                    │              │     │  │  shipments   │  │
                    │              │     │  │  otps        │  │
                    └──────┬───────┘     │  │  coupons     │  │
                           │             │  │  inventory   │  │
              ┌────────────┼──────┐      │  └──────────────┘  │
              ▼            ▼      ▼      └────────────────────┘
     ┌──────────────┐ ┌────────┐ ┌──────────┐
     │ notification │ │Razorpay│ │Shiprocket│
     │  -service    │ │  API   │ │   API    │
     │ (SMS/Email)  │ │        │ │          │
     └──────────────┘ └────────┘ └──────────┘
```

### Microservices Architecture

| Service | Port | Responsibility |
|---------|------|---------------|
| `ecom-core-service` | 8080 | Products, Cart, Orders, Payments, Shipping, Auth |
| `notification-service` | 9090 | SMS, Email, Push notifications (async) |
| `ecom-store` | 3000 | Customer-facing React storefront |
| `admin-panel` | 3001 | Admin dashboard React app |

---

## 2. Database Schema (MongoDB)

### 2.1 `users` Collection

```javascript
{
  _id: "uuid",                    // Primary key (UUID)
  phone: "+919876543210",         // Unique, used for OTP login
  email: "user@example.com",     // Optional
  name: "John Doe",              // Optional
  is_admin: false,               // Admin flag (set manually in DB)
  created_at: ISODate(),
  updated_at: ISODate()
}
```

**Indexes:** `{ phone: 1 }` (unique)

> **Design Decision:** Phone-first auth via OTP. No passwords stored. Admin flag is boolean on the same user model — simple but effective for small teams.

---

### 2.2 `products` Collection

```javascript
{
  _id: "uuid",
  name: "Arduino Uno R3 Board",
  slug: "arduino-uno-r3-board",           // Auto-generated from name
  description: "Official Arduino...",
  category: "Development Boards",          // String reference to category name
  price: 54900,                            // ₹549.00 in PAISE (integer)
  compare_at_price: 79900,                 // ₹799.00 — strike-through price
  images: [                                // Array of image URLs
    "https://s3.../products/abc/img1.jpg",
    "https://s3.../products/abc/img2.jpg"
  ],
  thumbnail: "https://s3.../products/abc/thumb.jpg",  // Primary display image
  variants: [                              // Product variants
    {
      id: "uuid",
      name: "Color",                       // Variant attribute name
      value: "Blue",                       // Variant attribute value
      stock: 25,                           // Per-variant stock
      price: 59900                         // Price override (optional)
    }
  ],
  tags: ["arduino", "microcontroller", "development"],
  is_active: true,                         // Soft delete flag
  stock: 100,                              // Total available stock
  sku: "ARD-UNO-R3",                       // Stock Keeping Unit
  weight: 50,                              // Weight in grams
  created_at: ISODate(),
  updated_at: ISODate()
}
```

**Indexes:**
- `{ slug: 1 }` (unique)
- `{ category: 1 }`
- `{ is_active: 1, created_at: -1 }` (listing query)
- `{ name: "text", description: "text", tags: "text" }` (full-text search)

**Key Design Decisions:**
- **Price in Paise (integer):** Avoids floating-point precision issues. ₹549.00 = `54900` paise. All price calculations are integer arithmetic.
- **Soft Delete:** `is_active: false` instead of actual deletion. Orders reference product IDs, so we can't delete products that have been ordered.
- **Category as String:** Simple and denormalized. For a store with <100 categories, this is faster than join lookups.
- **Variants as Embedded Array:** Keeps product + variants in a single document. No joins needed for product detail pages.

---

### 2.3 `categories` Collection

```javascript
{
  _id: "uuid",
  name: "Development Boards",
  slug: "development-boards",
  parent_id: "",                 // For nested categories (future)
  image: "https://s3.../cat/dev-boards.jpg",
  is_active: true
}
```

**Indexes:** `{ slug: 1 }` (unique), `{ is_active: 1 }`

---

### 2.4 `carts` Collection

```javascript
{
  _id: "uuid",
  user_id: "user-uuid",         // One cart per user
  items: [
    {
      product_id: "product-uuid",
      variant_id: "",            // Optional variant selection
      name: "Arduino Uno R3",    // Denormalized for fast reads
      price: 54900,              // Price at time of add (snapshot)
      quantity: 2,
      image: "https://..."       // Thumbnail for cart display
    }
  ],
  created_at: ISODate(),
  updated_at: ISODate()
}
```

**Indexes:** `{ user_id: 1 }` (unique — one cart per user)

**Design Decision:** Cart items store denormalized product data (name, price, image). This means:
- Cart page loads **don't need to join** with products collection
- Price is **snapshot at add-time** (can drift from current price — acceptable for short-lived carts)

---

### 2.5 `orders` Collection

```javascript
{
  _id: "uuid",
  user_id: "user-uuid",
  order_number: "ORD-1234567890",    // Human-readable order number
  items: [
    {
      product_id: "product-uuid",
      variant_id: "",
      name: "Arduino Uno R3",
      sku: "ARD-UNO-R3",
      price: 54900,                   // Price at purchase time (LOCKED)
      quantity: 2,
      image: "https://..."
    }
  ],
  subtotal: 109800,                   // Sum of (price × qty) in paise
  shipping_cost: 0,                   // Free if subtotal > ₹500 (50000p)
  discount: 5000,                     // Coupon discount in paise
  total: 104800,                      // subtotal - discount + shipping
  coupon_code: "SAVE50",
  status: "placed",                   // Order lifecycle status
  payment_status: "pending",          // Payment status
  payment_method: "razorpay",
  payment_id: "pay_xxxxx",           // Razorpay payment ID
  shipping_address: {
    name: "John Doe",
    line1: "123 MG Road",
    line2: "Near Metro Station",
    city: "Bangalore",
    state: "Karnataka",
    pincode: "560001",
    country: "India",
    phone: "+919876543210"
  },
  billing_address: { ... },           // Optional, defaults to shipping
  tracking_id: "AWB123456",
  tracking_url: "https://track.../AWB123456",
  shipment_id: "SHIP-xxxx",
  notes: "Please deliver before 5pm",
  created_at: ISODate(),
  updated_at: ISODate()
}
```

**Indexes:**
- `{ user_id: 1, created_at: -1 }` (user's order history)
- `{ order_number: 1 }` (unique)
- `{ status: 1 }` (admin filtering)
- `{ payment_status: 1 }` (revenue calculation)

**Order Status Lifecycle:**
```
placed → confirmed → processing → shipped → delivered
                                          → returned
         → cancelled (can happen from placed/confirmed)
```

**Payment Status:** `pending → paid → refunded → failed`

---

### 2.6 `payments` Collection

```javascript
{
  _id: "uuid",
  order_id: "order-uuid",
  user_id: "user-uuid",
  razorpay_order_id: "order_xxxx",      // Razorpay order ID
  razorpay_payment_id: "pay_xxxx",      // Filled after payment
  razorpay_signature: "hmac_xxx",       // For verification
  amount: 104800,                        // In paise
  currency: "INR",
  method: "upi",                         // Payment method used
  status: "created",                     // created → paid → failed
  created_at: ISODate(),
  updated_at: ISODate()
}
```

**Indexes:** `{ order_id: 1 }`, `{ razorpay_order_id: 1 }`

---

### 2.7 `shipments` Collection

```javascript
{
  _id: "uuid",
  order_id: "order-uuid",
  provider: "shiprocket",               // Shipping provider
  shipment_id: "SR-xxxx",               // Provider's shipment ID
  awb: "AWB1234567890",                 // Air Waybill number
  tracking_url: "https://...",
  status: "created",                     // created → booked → in_transit → delivered
  est_delivery: "2026-05-05",
  weight: 500,                           // grams
  length: 30, width: 25, height: 5,     // cm
  created_at: ISODate(),
  updated_at: ISODate()
}
```

---

### 2.8 `coupons` Collection

```javascript
{
  _id: "uuid",
  code: "SAVE50",
  type: "percentage",                    // "percentage" or "fixed"
  value: 10,                             // 10% or ₹10 (in paise for fixed)
  min_order: 50000,                      // Minimum order ₹500
  max_discount: 20000,                   // Max ₹200 discount
  usage_limit: 100,                      // Total uses allowed
  used_count: 42,                        // Current usage count
  is_active: true,
  expires_at: ISODate(),
  created_at: ISODate()
}
```

---

### 2.9 `inventory` Collection (Future Enhancement)

```javascript
{
  _id: "uuid",
  product_id: "product-uuid",
  variant_id: "",
  sku: "ARD-UNO-R3",
  stock: 100,                            // Available quantity
  reserved: 5,                           // Reserved for pending orders
  warehouse: "warehouse-1",
  updated_at: ISODate()
}
```

> **Current State:** Inventory is tracked directly on the `products.stock` field. The separate `inventory` collection is modeled but not yet wired into handlers. This is fine for single-warehouse operations.

---

### Entity Relationship Diagram

```
┌──────────┐       ┌──────────┐       ┌──────────┐
│   User   │──1:1──│   Cart   │       │ Category │
│          │       │  (items) │       │          │
└────┬─────┘       └──────────┘       └──────────┘
     │ 1:N                                  │
     ▼                                      │ referenced by
┌──────────┐       ┌──────────┐       ┌─────▼────┐
│  Order   │──1:1──│ Payment  │       │ Product  │
│  (items) │       │          │       │(variants)│
└────┬─────┘       └──────────┘       └──────────┘
     │ 1:1
     ▼
┌──────────┐       ┌──────────┐
│ Shipment │       │  Coupon  │
│          │       │          │
└──────────┘       └──────────┘
```

---

## 3. Product Upload Strategy

### 3.1 Current Upload Methods

#### Method A: Single Product (Admin Panel Form → JSON API)

```
Admin Panel Form → POST /api/v1/admin/products → MongoDB
```

**Request Body:**
```json
{
  "name": "Arduino Uno R3 Board",
  "description": "Official Arduino Uno...",
  "category": "Development Boards",
  "price": 54900,
  "compare_at_price": 79900,
  "stock": 100,
  "sku": "ARD-UNO-R3",
  "weight": 50,
  "tags": ["arduino", "microcontroller"],
  "thumbnail": "https://s3.../products/thumb.jpg",
  "images": ["https://s3.../img1.jpg", "https://s3.../img2.jpg"],
  "variants": [
    { "name": "Color", "value": "Blue", "stock": 50, "price": 0 },
    { "name": "Color", "value": "Red", "stock": 50, "price": 0 }
  ]
}
```

**Backend Processing:**
1. Validate required fields (name, price, category)
2. Generate UUID for `_id`
3. Auto-generate `slug` from name (e.g., "arduino-uno-r3-board")
4. Set `is_active: true`
5. Set timestamps
6. Initialize empty arrays if nil (images, variants)
7. Insert into MongoDB

#### Method B: Bulk Upload (CSV)

```
Admin Panel → POST /api/v1/admin/products/upload-csv (multipart/form-data)
```

**CSV Format:**
```csv
name,price,category,description,stock,sku,weight,compare_at_price,tags,thumbnail,images
Arduino Uno R3,54900,Development Boards,Official board,100,ARD-UNO-R3,50,79900,arduino|microcontroller,https://s3.../thumb.jpg,https://s3.../img1.jpg|https://s3.../img2.jpg
```

**Rules:**
- Required columns: `name`, `price`, `category`
- Multi-value fields use `|` separator (tags, images)
- Price in paise (integer)
- Returns: `{ created: N, failed: N, errors: [...] }`

---

### 3.2 Recommended Upload Flow (Enhanced)

For the **"I only have an image, price, inventory, and category"** scenario, here's the recommended streamlined flow:

```
┌─────────────────────────────────────────────────────────┐
│                    ADMIN PANEL                          │
│                                                         │
│  ┌─────────────┐                                        │
│  │ Upload Image │──────┐                                │
│  │ (drag/drop)  │      │                                │
│  └─────────────┘      ▼                                │
│  ┌─────────────┐  ┌──────────────────┐                  │
│  │ Enter Price  │  │ POST /upload     │                  │
│  │ Enter Stock  │  │ → S3 bucket      │                  │
│  │ Pick Category│  │ → Returns URL    │                  │
│  │ Enter Name   │  └────────┬─────────┘                  │
│  └──────┬──────┘           │                            │
│         │                  │                            │
│         ▼                  ▼                            │
│  ┌──────────────────────────────────────┐               │
│  │     POST /api/v1/admin/products      │               │
│  │                                      │               │
│  │  {                                   │               │
│  │    name: "Product Name",             │               │
│  │    price: 54900,                     │               │
│  │    category: "Electronics",          │               │
│  │    stock: 100,                       │               │
│  │    thumbnail: "<S3_URL>",            │               │
│  │    images: ["<S3_URL>"],             │               │
│  │  }                                   │               │
│  └──────────────────────────────────────┘               │
│                        │                                │
│                        ▼                                │
│              Backend Auto-Generates:                    │
│              • UUID (_id)                               │
│              • Slug from name                           │
│              • SKU (if not provided)                    │
│              • is_active = true                         │
│              • timestamps                               │
│              • Empty variants/tags arrays               │
└─────────────────────────────────────────────────────────┘
```

---

## 4. Image Upload & Storage Pipeline

### 4.1 Current State

Currently, product images are stored as **URL strings** in the database. The backend expects the admin to provide pre-uploaded image URLs. There is **no image upload endpoint** in the current codebase.

### 4.2 Recommended: S3 Pre-Signed URL Approach

This is the **industry standard** used by Shopify, Amazon, Flipkart, etc.

```
┌──────────────┐          ┌─────────────────┐          ┌──────────┐
│ Admin Panel   │─── 1 ──▶│ Backend (Go)    │─── 2 ──▶│ AWS S3   │
│ (Browser)     │          │ /admin/upload/  │          │          │
│               │◀── 3 ───│ presigned-url   │          │          │
│               │          └─────────────────┘          │          │
│               │──── 4 ── Direct Upload ──────────────▶│          │
│               │◀── 5 ── Upload Complete ─────────────│          │
│               │                                       └──────────┘
│               │─── 6 ──▶ POST /admin/products
│               │          (with S3 URLs in body)
└──────────────┘
```

**Step-by-step:**

1. **Admin Panel** requests a pre-signed URL from backend
2. **Backend** generates an S3 pre-signed PUT URL (valid for 15 min)
3. **Backend** returns the pre-signed URL + the final public URL
4. **Admin Panel** uploads the image **directly to S3** (bypasses backend)
5. Upload completes. Admin Panel now has the S3 public URL
6. **Admin Panel** sends product creation request with the S3 URL in `thumbnail` / `images`

### 4.3 New Endpoint Needed: `POST /api/v1/admin/upload/presigned-url`

```go
// Request
{ "filename": "product-image.jpg", "content_type": "image/jpeg" }

// Response
{
  "upload_url": "https://s3.ap-south-1.amazonaws.com/bucket/products/uuid/product-image.jpg?X-Amz-...",
  "public_url": "https://s3.ap-south-1.amazonaws.com/bucket/products/uuid/product-image.jpg",
  "expires_in": 900
}
```

### 4.4 Why Pre-Signed URLs?

| Approach | Pros | Cons |
|----------|------|------|
| **Pre-signed URL (✅ recommended)** | No backend bandwidth used, supports large files, client-side progress bar | Requires S3 setup |
| Multipart upload to backend → S3 | Simpler client code | Backend becomes bottleneck, memory pressure, timeout issues |
| Base64 in JSON body | Simplest | 33% size overhead, request size limits, terrible for multiple images |

### 4.5 Image Processing (Future)

After upload to S3, trigger a **Lambda function** or **background job** to:
- Generate thumbnail (300×300)
- Generate medium (800×800)
- Generate large (1200×1200)
- Convert to WebP format
- Strip EXIF metadata

Store all sizes in the product:
```json
{
  "images": [
    {
      "original": "https://s3.../products/uuid/original.jpg",
      "thumbnail": "https://s3.../products/uuid/thumb.webp",
      "medium": "https://s3.../products/uuid/medium.webp",
      "large": "https://s3.../products/uuid/large.webp"
    }
  ]
}
```

---

## 5. Admin Product Management Flow

### 5.1 Complete Admin Product CRUD

```
┌────────────────────────────────────────────────────────────────┐
│                      ADMIN WORKFLOWS                           │
├────────────────────────────────────────────────────────────────┤
│                                                                │
│  CREATE PRODUCT                                                │
│  ─────────────                                                 │
│  Admin Panel Form → Upload Image(s) → Fill Details →           │
│  POST /admin/products → Product Created (is_active=true)       │
│                                                                │
│  UPDATE PRODUCT                                                │
│  ──────────────                                                │
│  Admin Panel → Edit Form → PUT /admin/products/:id →           │
│  Partial update (only changed fields)                          │
│                                                                │
│  DELETE PRODUCT (Soft)                                          │
│  ─────────────────────                                         │
│  DELETE /admin/products/:id →                                  │
│  Sets is_active=false (product hidden from storefront)          │
│  Still exists in DB (orders can reference it)                  │
│                                                                │
│  BULK UPLOAD (CSV)                                             │
│  ────────────────                                              │
│  Upload CSV → POST /admin/products/upload-csv →                │
│  Returns { created: N, failed: N, errors: [] }                 │
│                                                                │
│  EXPORT PRODUCTS                                               │
│  ───────────────                                               │
│  GET /admin/products/export-csv → Downloads CSV file           │
│                                                                │
│  MANAGE CATEGORIES                                             │
│  ────────────────                                              │
│  POST /admin/categories → Create new category                  │
│  (Categories are simple: name + slug + image + is_active)      │
│                                                                │
└────────────────────────────────────────────────────────────────┘
```

### 5.2 Auto-Generated Fields

When creating a product, the backend automatically handles:

| Field | Source | Example |
|-------|--------|---------|
| `_id` | `uuid.New()` | `"a1b2c3d4-..."` |
| `slug` | Slugified from `name` | `"arduino-uno-r3-board"` |
| `is_active` | Always `true` on create | `true` |
| `created_at` | `time.Now()` | `2026-04-28T14:00:00Z` |
| `updated_at` | `time.Now()` | `2026-04-28T14:00:00Z` |
| `images` | Default `[]` if nil | `[]` |
| `variants` | Default `[]` if nil | `[]` |

---

## 6. Order Creation Flow

### 6.1 Complete Order Lifecycle

```
┌──────────────────────────────────────────────────────────────┐
│                                                              │
│  CUSTOMER JOURNEY                                            │
│                                                              │
│  1. Browse Products (GET /products)                          │
│     └─▶ 2. Add to Cart (POST /cart/add)                     │
│           └─▶ 3. View Cart (GET /cart)                       │
│                 └─▶ 4. Checkout                              │
│                       ├─▶ Enter Shipping Address             │
│                       ├─▶ Apply Coupon (optional)            │
│                       └─▶ 5. Place Order (POST /orders)     │
│                             │                                │
│     ┌───────────────────────┘                                │
│     │  Backend Order Creation:                               │
│     │  a. Fetch cart items                                   │
│     │  b. Calculate subtotal (Σ price × qty)                 │
│     │  c. Validate & apply coupon                            │
│     │  d. Calculate shipping (free if > ₹500)                │
│     │  e. total = subtotal - discount + shipping             │
│     │  f. Create order document                              │
│     │  g. Clear user's cart                                  │
│     │  h. Deduct product stock                               │
│     │  i. Send SMS notification                              │
│     │  j. Return order to client                             │
│     │                                                        │
│     └─▶ 6. Initiate Payment (POST /payment/create)          │
│           │  Backend creates Razorpay order                  │
│           │  Returns: razorpay_order_id, key_id, amount      │
│           │                                                  │
│           └─▶ 7. Razorpay Checkout (client-side)             │
│                 │  Customer completes payment                │
│                 │                                            │
│                 └─▶ 8. Verify Payment (POST /payment/verify) │
│                       │  Backend verifies HMAC signature     │
│                       │  Updates payment status = "paid"     │
│                       │  Updates order status = "confirmed"  │
│                       │  Sends payment confirmation SMS      │
│                       │                                      │
│                       └─▶ 9. Admin ships order               │
│                             POST /admin/shipping/create      │
│                             → Shiprocket API called          │
│                             → AWB & tracking assigned        │
│                             → Order status = "shipped"       │
│                             │                                │
│                             └─▶ 10. Delivery                 │
│                                   Order status = "delivered" │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

### 6.2 Order Price Calculation (Backend)

```go
// All amounts in PAISE (integer arithmetic, no floating point)

subtotal := 0
for _, item := range cartItems {
    subtotal += item.Price * item.Quantity    // e.g., 54900 × 2 = 109800
}

// Apply coupon
discount := 0
if coupon.Type == "percentage" {
    discount = subtotal * coupon.Value / 100   // e.g., 109800 × 10 / 100 = 10980
    if discount > coupon.MaxDiscount {
        discount = coupon.MaxDiscount           // Cap at max discount
    }
} else if coupon.Type == "fixed" {
    discount = coupon.Value                    // e.g., 5000 (₹50)
}

// Shipping
shippingCost := 0
if subtotal < 50000 {                         // Less than ₹500
    shippingCost = 5000                        // ₹50 shipping
}

total := subtotal - discount + shippingCost
// 109800 - 10980 + 0 = 98820 paise = ₹988.20
```

### 6.3 Stock Deduction

```go
// Happens atomically per product after order creation
for _, item := range orderItems {
    db.Collection("products").UpdateOne(ctx,
        bson.M{"_id": item.ProductID},
        bson.M{"$inc": bson.M{"stock": -item.Quantity}},
    )
}
```

> **⚠️ Known Issue:** Stock deduction is NOT atomic across all items. If the server crashes between deductions, some products may have incorrect stock. See [Roadmap](#13-whats-missing--roadmap) for the fix.

---

## 7. Payment Flow (Razorpay)

### 7.1 Two-Phase Payment

```
Phase 1: CREATE ORDER IN RAZORPAY
─────────────────────────────────
Client → POST /payment/create { order_id: "xxx" }
Backend → Creates Razorpay order (mock: generates order_xxxx ID)
Backend → Saves Payment document (status: "created")
Backend → Returns { razorpay_order_id, razorpay_key_id, amount }

Phase 2: PAYMENT + VERIFICATION
────────────────────────────────
Client → Opens Razorpay checkout widget
Customer → Pays via UPI/Card/NetBanking
Razorpay → Returns { razorpay_payment_id, razorpay_signature }
Client → POST /payment/verify { razorpay_order_id, razorpay_payment_id, razorpay_signature }
Backend → Verifies HMAC-SHA256 signature
Backend → Updates payment status = "paid"
Backend → Updates order { payment_status: "paid", status: "confirmed" }
Backend → Sends SMS confirmation
```

### 7.2 Razorpay Webhook (Backup)

```
POST /payment/webhook (public endpoint)
Razorpay → Sends event "payment.captured"
Backend → Updates payment + order status
```

> **Purpose:** Even if the client's verify call fails (network issue, browser closed), the webhook ensures payment is captured.

### 7.3 Signature Verification

```go
// HMAC-SHA256(razorpay_order_id|razorpay_payment_id, razorpay_secret)
expected := hmacSHA256(orderID + "|" + paymentID, secret)
if expected != signature {
    return error("Invalid signature — possible tampering")
}
```

---

## 8. Pricing Strategy

### 8.1 Price Storage Rules

| Field | Purpose | Example |
|-------|---------|---------|
| `price` | **Selling price** (what customer pays) | `54900` (₹549) |
| `compare_at_price` | **MRP / Original price** (crossed out) | `79900` (₹799) |
| `variant.price` | **Variant price override** (0 = use base) | `59900` (₹599) |

### 8.2 Discount Calculation

```javascript
// Frontend display
discount_percent = Math.round((1 - price / compare_at_price) * 100)
// (1 - 549/799) × 100 = 31% OFF

// Shows: ₹549  ₹̶7̶9̶9̶  31% OFF
```

### 8.3 Variant Pricing

When a product has variants with price overrides:
```javascript
// Cart add logic (backend)
price := product.Price                    // Base price: ₹549
for _, variant := range product.Variants {
    if variant.ID == selectedVariantID && variant.Price > 0 {
        price = variant.Price             // Override: ₹599
    }
}
// Cart item stores ₹599
```

### 8.4 Price Integrity

- **Cart:** Stores price at time of add (snapshot). If product price changes later, cart price stays.
- **Order:** Locks in prices from cart at order creation time. These are the legal purchase prices.
- **Refund:** Uses the locked order item price, not current product price.

---

## 9. Inventory Management

### 9.1 Current Implementation

```
Product.stock (integer) — simple stock counter on the product document
```

**Stock Operations:**
| Event | Operation |
|-------|-----------|
| Admin creates product | `stock = N` (set via API/CSV) |
| Admin updates product | `PUT /admin/products/:id { stock: N }` |
| Customer places order | `$inc: { stock: -quantity }` per item |
| No stock reservation | Cart doesn't reserve stock |

### 9.2 Stock Check Flow

```
1. Customer adds to cart
   └─▶ Check: product.stock >= requested_quantity
       ├─▶ Yes → Add to cart
       └─▶ No  → "Insufficient stock" error

2. Customer places order
   └─▶ No re-check (trusts cart add check)
       └─▶ Deducts stock after order insert
```

### 9.3 Recommended Improvements

**Stock Reservation (TODO):**
```
Cart Add → Reserve stock (inventory.reserved += qty)
Cart Remove → Release stock (inventory.reserved -= qty)
Order Create → Convert reserved → sold (stock -= qty, reserved -= qty)
Cart Expiry (30 min) → Release reserved stock
```

**Low Stock Alerts (TODO):**
```
if product.stock <= threshold {
    SendNotification("PUSH", admin_token, "Low stock: " + product.name)
}
```

---

## 10. Shipping & Fulfillment

### 10.1 Current Flow

```
1. Admin → POST /admin/shipping/create { order_id, weight, dimensions }
2. Backend:
   a. Fetch order from DB
   b. If Shiprocket token configured:
      → Call Shiprocket API /orders/create/adhoc
      → Get shipment_id, AWB, tracking URL
   c. Else (development/mock):
      → Generate mock shipment_id, AWB, tracking URL
   d. Save Shipment document
   e. Update order: tracking_id, tracking_url, status = "shipped"
3. Customer → GET /shipping/track/:orderId
   → Returns shipment details
```

### 10.2 Shiprocket Integration

```
Shiprocket API: https://apiv2.shiprocket.in/v1/external/
Auth: Bearer token (from email/password login)

Payload includes:
- Order details (number, date, items, amounts)
- Customer address (name, address, city, pincode, state, phone)
- Package dimensions (length, breadth, height, weight)
- Payment method (Prepaid/COD)
```

### 10.3 Shipping Cost Logic

```go
// Current: Simple threshold-based
if subtotal < 50000 {    // Less than ₹500
    shippingCost = 5000  // ₹50 flat shipping
} else {
    shippingCost = 0     // Free shipping
}
```

**Recommended Enhancement:**
```go
// Weight-based + distance-based (via Shiprocket rate API)
// POST /external/courier/serviceability
// Returns available couriers with rates
```

---

## 11. API Reference

### Public Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/api/v1/auth/send-otp` | Send OTP to phone |
| `POST` | `/api/v1/auth/verify-otp` | Verify OTP, get JWT token |
| `GET` | `/api/v1/products` | List products (paginated, searchable) |
| `GET` | `/api/v1/products/:id` | Get product by ID or slug |
| `GET` | `/api/v1/categories` | List active categories |
| `POST` | `/api/v1/payment/webhook` | Razorpay webhook |

### Authenticated Endpoints (requires `Authorization: Bearer <JWT>`)

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/cart` | Get user's cart |
| `POST` | `/cart/add` | Add item to cart |
| `PUT` | `/cart/item/:productId` | Update item quantity |
| `DELETE` | `/cart/item/:productId` | Remove item from cart |
| `DELETE` | `/cart` | Clear entire cart |
| `POST` | `/orders` | Create order from cart |
| `GET` | `/orders` | List user's orders |
| `GET` | `/orders/:id` | Get order details |
| `POST` | `/payment/create` | Create Razorpay payment |
| `POST` | `/payment/verify` | Verify Razorpay payment |
| `GET` | `/shipping/track/:orderId` | Track shipment |
| `GET` | `/user` | Get user profile |

### Admin Endpoints (requires JWT + `is_admin: true`)

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/admin/products` | Create product |
| `PUT` | `/admin/products/:id` | Update product |
| `DELETE` | `/admin/products/:id` | Soft-delete product |
| `POST` | `/admin/products/upload-csv` | Bulk upload via CSV |
| `GET` | `/admin/products/export-csv` | Export products as CSV |
| `POST` | `/admin/categories` | Create category |
| `GET` | `/admin/orders` | List all orders (with status filter) |
| `PUT` | `/admin/order/:id/status` | Update order status |
| `POST` | `/admin/shipping/create` | Create shipment |
| `GET` | `/admin/users` | List all users |
| `GET` | `/admin/stats` | Dashboard statistics |

### Middleware Stack

```
Request → CORS → Rate Limiter (60/min) → Logger → Recovery
                                         ↓
                              Auth Middleware (JWT) → Admin Middleware
                                         ↓
                                      Handler
```

---

## 12. Architecture Decisions

### Why MongoDB?

| Decision | Rationale |
|----------|-----------|
| **Flexible schema** | Products have varying attributes; schema-less is ideal |
| **Embedded documents** | Cart items, order items, variants — no joins needed |
| **Horizontal scaling** | Sharding ready for high traffic |
| **JSON-native** | Direct mapping to Go structs and API responses |

### Why Paise (Integer) for Prices?

```
₹549.99 → 54999 paise

// BAD (floating point)
0.1 + 0.2 = 0.30000000000000004

// GOOD (integer)
10 + 20 = 30 (always exact)
```

### Why UUID over MongoDB ObjectID?

| UUID | ObjectID |
|------|----------|
| URL-friendly | Contains hex + timestamp |
| No vendor lock-in | MongoDB-specific |
| Can generate client-side | Must generate server-side |
| Consistent across services | Varies per driver |

### Why Denormalized Cart/Order Items?

```javascript
// Cart item stores product name, price, image directly
// NOT just a product_id reference

// Pros:
// 1. Cart page loads = single DB query (no product join)
// 2. Order items preserve historical prices
// 3. Product can be deleted without breaking order history

// Cons:
// 1. Data duplication
// 2. Cart name/image can drift (acceptable)
```

---

## 13. What's Missing & Roadmap

### ⚡ Priority 1 — Must Have for Production

| Feature | Status | Effort |
|---------|--------|--------|
| **Image Upload Endpoint** (S3 pre-signed URLs) | ❌ Not built | 2-3 hours |
| **Auto-generate SKU** if not provided | ❌ Missing | 30 min |
| **Input validation** (price > 0, stock >= 0) | ⚠️ Minimal | 1 hour |
| **Atomic stock deduction** (MongoDB transactions) | ❌ Race condition risk | 2 hours |
| **Stock check on order creation** | ❌ Missing | 1 hour |
| **MongoDB indexes** (performance) | ❌ Not created | 1 hour |
| **Error handling** improvements | ⚠️ Basic | 2 hours |
| **Admin: Edit/Delete categories** | ❌ Missing | 1 hour |

### 🔧 Priority 2 — Important for Growth

| Feature | Status | Effort |
|---------|--------|--------|
| **Coupon management API** (CRUD) | ❌ Only model exists | 2 hours |
| **Order cancellation & refund** | ❌ Missing | 3 hours |
| **User address book** (save multiple addresses) | ❌ Missing | 2 hours |
| **Wishlist** | ❌ Missing | 2 hours |
| **Product reviews & ratings** | ❌ Missing | 4 hours |
| **Inventory reservation system** | ❌ Missing | 4 hours |
| **Webhook for Shiprocket status updates** | ❌ Missing | 2 hours |
| **Email notifications** (order confirmation HTML) | ❌ Missing | 3 hours |
| **Admin: Revenue analytics** | ⚠️ Basic total only | 3 hours |

### 🚀 Priority 3 — Scale & Polish

| Feature | Status | Effort |
|---------|--------|--------|
| **Redis caching** (product listings, categories) | ❌ Config exists, not used | 3 hours |
| **Full-text search** (MongoDB text index or Elasticsearch) | ⚠️ Regex only | 2 hours |
| **Image CDN** (CloudFront) | ❌ Missing | 2 hours |
| **Background job queue** (stock alerts, email digests) | ❌ Missing | 4 hours |
| **Rate limiting per user** (not just IP) | ⚠️ IP only | 1 hour |
| **API versioning** (v2 endpoints) | ✅ Already /api/v1 | — |
| **Health checks** with DB ping | ⚠️ Basic | 30 min |
| **Logging** (structured, ELK/CloudWatch) | ❌ stdout only | 3 hours |
| **Metrics** (Prometheus) | ❌ Missing | 2 hours |

---

## Appendix A: Environment Variables

```bash
# Server
PORT=8080
ENVIRONMENT=development    # development | production

# Database
MONGO_URI=mongodb://localhost:27017/ecom
MONGO_DB=ecom
REDIS_ADDR=localhost:6379

# Auth
JWT_SECRET=your-jwt-secret
OTP_SERVICE=mock           # mock | msg91

# OTP (MSG91)
MSG91_AUTH_KEY=
MSG91_TEMPLATE_ID=

# Payment (Razorpay)
RAZORPAY_KEY_ID=rzp_test_xxxx
RAZORPAY_KEY_SECRET=xxxx

# Shipping (Shiprocket)
SHIPROCKET_EMAIL=
SHIPROCKET_PASSWORD=
SHIPROCKET_TOKEN=

# Notifications
NOTIFICATION_SERVICE_URL=http://localhost:9090

# Storage (S3)
S3_BUCKET=your-bucket-name
S3_REGION=ap-south-1
```

---

## Appendix B: Quick Start Commands

```bash
# Start MongoDB
mongosh --eval "use ecom"

# Run the service
cd ecom-core-service
go run cmd/api/main.go

# Create admin user
mongosh ecom --eval 'db.users.updateOne({phone: "+919999999999"}, {$set: {is_admin: true}})'

# Seed a category
curl -X POST http://localhost:8080/api/v1/admin/categories \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"name": "Development Boards", "image": ""}'

# Create a product
curl -X POST http://localhost:8080/api/v1/admin/products \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "Arduino Uno R3",
    "price": 54900,
    "category": "Development Boards",
    "stock": 100,
    "sku": "ARD-UNO-R3"
  }'
```

---

*This document serves as the single source of truth for the ecom-core-service backend architecture. Update it as features are added.*
