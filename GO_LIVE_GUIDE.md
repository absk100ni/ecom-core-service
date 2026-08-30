# ElectroMart — Production Go-Live Guide (Beginner Edition)

> This guide assumes you know **nothing** about these platforms. Each section explains
> what the service is, why your store needs it, what it costs, exactly how to sign up,
> and exactly which value to copy into which config file.
>
> **Prices are indicative (July 2026, INR).** Always confirm on the provider's pricing
> page before paying — plans and rates change.

---

## 0. The Big Picture

Your store is 3 applications:

| App | What it is | Folder |
|---|---|---|
| **Storefront** | The website customers see and buy from | `ecom-store` |
| **Admin panel** | Your private dashboard: products, orders, shipping, refunds | `admin-panel` |
| **Backend** | The engine both talk to: database, payments, auth, invoices | `ecom-core-service` |

To go live, you plug real services into the backend via a file called `.env`
(a simple list of `KEY=value` lines — copy `.env.example` to `.env` and fill it in).
The storefront has its own small `.env` for things the browser needs.

**Everything has a fallback**: if a credential is missing, that one feature degrades
(e.g. emails just print to logs) but the rest keeps working. You can go live
incrementally.

### Priority legend used below
- 🔴 **MUST** — store cannot take real orders without it
- 🟡 **SHOULD** — works without it, but a core experience silently degrades
- 🟢 **NICE** — optimization / marketing

---

## 1. Domain Name 🔴

**What it is:** Your address on the internet, e.g. `electromart.in`.

**Cost:** ₹800–1,500/year (`.in` or `.com`).

**Steps:**
1. Go to a registrar — **Namecheap** (namecheap.com), **GoDaddy** (godaddy.com), or
   **Cloudflare Registrar** (cloudflare.com — cheapest renewal, recommended).
2. Search your name, pay for 1 year.
3. Create a free **Cloudflare** account (cloudflare.com) and add your domain to it.
   Cloudflare gives you free HTTPS (the padlock), free CDN, and DDoS protection.
   It will show you two "nameservers" — set those at your registrar (a copy-paste step;
   every registrar has a "Nameservers" setting).

**You'll create these subdomains later (in Cloudflare → DNS):**
- `yourstore.com` → storefront
- `admin.yourstore.com` → admin panel
- `api.yourstore.com` → backend

---

## 2. Server Hosting (VPS) 🔴

**What it is:** A rented computer in a datacenter that runs your backend 24/7.
"VPS" = Virtual Private Server.

**Cost:** ₹500–1,000/month for a 2GB machine (plenty to start).

**Recommended: DigitalOcean** (beginner-friendly), alternatives: AWS Lightsail, Hetzner (cheapest).

**Steps (DigitalOcean):**
1. Sign up at digitalocean.com (needs a credit/debit card).
2. Create a **Droplet**: choose **Ubuntu 24.04**, the **$6–8/mo (2GB)** size,
   **Bangalore** region (closest to Indian customers).
3. It gives you an IP address like `139.59.x.x`. In Cloudflare DNS, point
   `api.yourstore.com` at this IP (an "A record").
4. You'll copy the backend onto this machine and run it (I can automate this whole
   step with a deploy script / Docker when you're ready).

**What goes in `.env`:** nothing directly — this machine is *where* `.env` lives.

---

## 3. MongoDB Atlas — Database 🔴

**What it is:** The database that stores products, users, orders — everything.
"Atlas" is MongoDB's cloud service: they run and back up the database for you,
which is much safer than running it yourself.

**Cost:** **FREE** to start (M0 tier, 512MB — enough for thousands of products).
Upgrade to M10 (~₹4,800/mo) only when you have real scale.

**Steps:**
1. Sign up at mongodb.com/atlas → "Build a Database" → choose **M0 FREE**,
   provider **AWS**, region **Mumbai (ap-south-1)**.
2. Create a database user: Security → Database Access → Add New Database User.
   Pick a username + strong password. **Write these down.**
3. Security → Network Access → Add IP Address → add your VPS's IP
   (never "allow from anywhere" in production).
4. Click **Connect → Drivers** and copy the "connection string" — looks like:
   `mongodb+srv://USERNAME:PASSWORD@cluster0.xxxxx.mongodb.net/ecom`

**What goes in backend `.env`:**
```
MONGO_URI=mongodb+srv://USERNAME:PASSWORD@cluster0.xxxxx.mongodb.net/ecom?retryWrites=true&w=majority
MONGO_DB=ecom
```

---

## 4. Redis — Cache (Upstash) 🟡

**What it is:** A tiny super-fast memory store the backend uses for caching
(category tree, search results) and rate limiting (blocking spam/abuse).

**Cost:** **FREE** (Upstash free tier is plenty).

**Steps:**
1. Sign up at upstash.com → Create Database → region closest to your VPS.
2. Copy the endpoint (like `apn1-xxxx.upstash.io:6379`) and password.

**What goes in backend `.env`:**
```
REDIS_ADDR=apn1-xxxx.upstash.io:6379
```
(If your Redis needs a password/TLS, tell me — one small config addition.)
Leaving it blank disables caching — the store still works, just slower under load.

---

## 5. Stripe — Payments 🔴

**What it is:** The payment gateway. When a customer pays the advance online,
Stripe securely handles the card/UPI payment and deposits money to your bank account.

**Cost:** No monthly fee. ~**2% + GST per domestic transaction** (confirm your
account's exact rate). Money settles to your bank in ~2–7 days.

**⚠️ Important for India:** Stripe has historically restricted new Indian merchant
signups. Check that your account gets **activated for live payments** early — if
Stripe won't activate you, tell me and we flip the backend to Cashfree or Razorpay
(the code for both is already built in, it's a one-line `.env` change).

**Steps:**
1. Sign up at stripe.com → complete business verification (needs PAN, bank account,
   business details; GSTIN helps).
2. **Dashboard → Developers → API keys**: copy the **Secret key** (`sk_live_...`)
   and **Publishable key** (`pk_live_...`).
3. **Dashboard → Developers → Webhooks → Add endpoint**:
   - URL: `https://api.yourstore.com/api/v1/payment/webhook`
   - Events: `payment_intent.succeeded` and `payment_intent.payment_failed`
   - After creating, copy the **Signing secret** (`whsec_...`).

**What goes in backend `.env`:**
```
PAYMENT_GATEWAY=stripe
STRIPE_SECRET_KEY=sk_live_...
STRIPE_PUBLISHABLE_KEY=pk_live_...
STRIPE_WEBHOOK_SECRET=whsec_...
```

**Test first:** Stripe gives `sk_test_`/`pk_test_` keys too — use those + card
number `4242 4242 4242 4242` to do a full fake purchase before going live.

---

## 6. MSG91 — OTP Login SMS 🔴

**What it is:** Sends the "Your OTP is 123456" SMS when customers log in by phone.

**Cost:** ~₹0.15–0.25 per SMS. **One-time DLT registration ~₹5,900**
(an Indian-government requirement for sending SMS — MSG91 walks you through it).

**⏰ Start this FIRST — DLT approval takes several days.**

**Steps:**
1. Sign up at msg91.com → complete KYC.
2. Complete **DLT registration** (they guide you: register your business as
   "Principal Entity" + register an OTP template like
   *"Your ElectroMart OTP is ##OTP##"*).
3. **Dashboard → Developers → API Keys** → copy the Auth Key.
4. **OTP → Templates** → copy your approved Template ID.

**What goes in backend `.env`:**
```
OTP_SERVICE=msg91
MSG91_AUTH_KEY=your-auth-key
MSG91_TEMPLATE_ID=your-template-id
```

---

## 7. Google Sign-In 🔴

**What it is:** The "Continue with Google" button — lets customers log in with
their Gmail account in one tap.

**Cost:** **FREE**.

**Steps:**
1. Go to console.cloud.google.com (log in with any Google account).
2. Create a Project (name it anything, e.g. "ElectroMart").
3. **APIs & Services → OAuth consent screen**: choose "External", fill app name,
   support email. Publish it.
4. **APIs & Services → Credentials → Create Credentials → OAuth client ID**:
   - Type: **Web application**
   - Authorized JavaScript origins: `https://yourstore.com`
5. Copy the **Client ID** (ends in `.apps.googleusercontent.com`).

**What goes where (SAME value in both files):**
```
# backend .env
GOOGLE_CLIENT_ID=xxxx.apps.googleusercontent.com
# storefront .env
VITE_GOOGLE_CLIENT_ID=xxxx.apps.googleusercontent.com
```

---

## 8. AWS S3 — Product Image Storage 🔴

**What it is:** Amazon's file storage. When you upload a product photo in the
admin panel, it's stored in an S3 "bucket" and served to customers from there.

**Cost:** Effectively ₹100–300/month at your scale (storage ~$0.025/GB +
small bandwidth costs).

**Steps:**
1. Sign up at aws.amazon.com (needs card; there's a generous free tier for year 1).
2. **Create the bucket:** search "S3" in the console → Create bucket →
   name like `electromart-images`, region **ap-south-1 (Mumbai)** →
   under "Block Public Access" **uncheck blocking** (images must be publicly viewable)
   → create.
3. **Bucket → Permissions → Bucket policy** — paste (replace the bucket name):
```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow", "Principal": "*",
    "Action": "s3:GetObject",
    "Resource": "arn:aws:s3:::electromart-images/*"
  }]
}
```
4. **Bucket → Permissions → CORS** — paste (replace admin domain). Without this,
   browser uploads from the admin panel will fail:
```json
[{
  "AllowedHeaders": ["*"],
  "AllowedMethods": ["PUT", "GET"],
  "AllowedOrigins": ["https://admin.yourstore.com"],
  "ExposeHeaders": ["ETag"]
}]
```
5. **Create an access key:** search "IAM" → Users → Create user (`ecom-backend`,
   no console access) → Attach policies directly → create a custom policy allowing
   only `s3:PutObject` + `s3:GetObject` on `arn:aws:s3:::electromart-images/*` →
   then open the user → **Security credentials → Create access key** (choose
   "Application running outside AWS"). Copy the **Access key ID** and
   **Secret access key** — the secret is shown ONLY once.

**What goes in backend `.env`:**
```
S3_BUCKET=electromart-images
S3_REGION=ap-south-1
AWS_ACCESS_KEY_ID=AKIA...
AWS_SECRET_ACCESS_KEY=...
```

---

## 9. Shipmozo — Courier / Shipping 🔴

**What it is:** A courier aggregator. Instead of signing contracts with Delhivery,
Bluedart, etc. individually, Shipmozo gives you all of them under one account —
your admin panel's "Create Shipment" button books a pickup through it, including
COD (cash-on-delivery) collection for the balance amount.

**Cost:** No platform fee (typically). You pay per shipment: roughly
**₹40–90 for a 500g surface parcel**; COD collection adds ~₹40–50 or ~1.5–2% of
the collected amount. Rates depend on your volume — check your dashboard's rate card.

**Steps:**
1. Register as a seller at shipmozo.com → complete KYC (GSTIN, bank, pickup address).
2. Find **API keys** in the dashboard (Settings/Developers section) — copy the
   public and private key.
3. Set up their webhook (delivery status updates) pointing to
   `https://api.yourstore.com/api/v1/shipping/webhook` with a secret token you invent.

**What goes in backend `.env`:**
```
SHIPMOZO_PUBLIC_KEY=...
SHIPMOZO_PRIVATE_KEY=...
SHIPPING_WEBHOOK_TOKEN=any-long-random-string-you-invent
WAREHOUSE_PINCODE=your-real-pickup-pincode
```

**⚠️ First-shipment check:** our field mapping to their API is best-effort (their
docs sit behind the seller login). Book your first shipment as a test and we verify
the mapping together — it's isolated to one file if anything needs adjusting.

---

## 10. Email — Order Confirmations (Zoho Mail) 🟡

**What it is:** Sends "Order Confirmed", "Order Shipped", and refund emails to
customers, and contact-form submissions to you.

**Cost:** **FREE** (Zoho Mail free plan: 5 users on your own domain).
Alternative at scale: AWS SES (~₹9 per 1,000 emails, but requires a
"production access" approval request).

**Steps (Zoho):**
1. Sign up at zoho.com/mail → "Business email" with your own domain.
2. Verify your domain (they give you DNS records — add them in Cloudflare).
3. Create the mailbox `orders@yourstore.com`.
4. Zoho Mail settings → Security → **generate an "App password"**
   (SMTP won't accept your normal login password).

**What goes in backend `.env`:**
```
SMTP_HOST=smtp.zoho.in
SMTP_PORT=587
SMTP_USER=orders@yourstore.com
SMTP_PASS=the-app-password
SMTP_FROM=orders@yourstore.com
SMTP_FROM_NAME=ElectroMart
CONTACT_NOTIFY_EMAIL=you@yourstore.com
```

---

## 11. GSTIN — Tax Registration 🔴 (legal, not a platform)

**What it is:** Your GST number. Selling electronics online in India requires GST
registration, and your invoices legally must show it (the invoice generator we
built prints it on every invoice).

**Cost:** Registration is **FREE** on gst.gov.in (a CA typically charges
₹1,500–3,000 to do it for you). Note: payment gateways and Shipmozo KYC also
ask for it — get this early.

**What goes in backend `.env`:**
```
BUSINESS_LEGAL_NAME=Your Company Pvt Ltd / Your Name (Proprietor)
BUSINESS_ADDRESS=Full registered address
BUSINESS_GSTIN=27XXXXX1234X1Z5
BUSINESS_STATE=Maharashtra
BUSINESS_STATE_CODE=27
```
(State code = first 2 digits of your GSTIN.)

**Also fill the same business identity + a grievance officer (can be you) in the
storefront `.env`** — India's e-commerce rules require these displayed on the site:
```
VITE_BUSINESS_LEGAL_NAME=...   VITE_BUSINESS_ADDRESS=...
VITE_SUPPORT_EMAIL=...         VITE_SUPPORT_PHONE=...
VITE_GRIEVANCE_OFFICER_NAME=...  VITE_GRIEVANCE_OFFICER_EMAIL=...  VITE_GRIEVANCE_OFFICER_PHONE=...
VITE_BUSINESS_GSTIN=...
```

---

## 12b. Error Alerts & Uptime Monitoring 🟡 (both FREE)

**The problem these solve:** logs only help when you already know something is
wrong. These two services *tell you* — by email — the moment something breaks.

### Sentry — crash alerts (sentry.io)

**What it is:** When the backend hits an unexpected error, or the website crashes
in a customer's browser, Sentry emails you the full technical details immediately.

**Cost:** **FREE** — 5,000 error events/month. The apps are deliberately
engineered to stay far under this cap:
- Backend only reports **panics and 5xx errors** (never user mistakes like a wrong
  OTP), self-capped at **5 events/hour** with 1-hour duplicate suppression —
  mathematical worst case ~3,600/month, under the cap on its own.
- Frontends report **real crashes only**: performance tracing and session replay
  (the two big quota eaters) are disabled, browser-extension noise and flaky
  network errors are filtered out, and each visitor session caps at 10 events.
- Nothing is sent at all from dev/local builds — production only.

**Steps:**
1. Sign up at sentry.io → create 3 projects: `ecom-backend` (platform: Go),
   `ecom-store` (React), `ecom-admin` (React).
2. Each project shows a **DSN** (a URL like `https://abc123@o4507...ingest.sentry.io/450...`)
   under Settings → Client Keys.

**What goes where:**
```
# backend .env
SENTRY_DSN=<ecom-backend project DSN>
# storefront .env
VITE_SENTRY_DSN=<ecom-store project DSN>
# admin .env
VITE_SENTRY_DSN=<ecom-admin project DSN>
```
Leave any of them blank and that app simply doesn't report — nothing breaks.

### UptimeRobot — "is the site up?" alerts (uptimerobot.com)

**What it is:** Pings your site every 5 minutes from the internet and emails you
if it stops responding. No code involved at all.

**Cost:** **FREE** (up to 50 monitors — you need 3).

**Steps:**
1. Sign up at uptimerobot.com → Add New Monitor (type: HTTP(s)) three times:
   - `https://api.yourstore.com/health`   ← the backend heartbeat endpoint
   - `https://yourstore.com`
   - `https://admin.yourstore.com`
2. Set alert contact to your email (and the free mobile app for push alerts).

---

## 12. Meta Pixel + Google Analytics 🟢 (but HIGH value for you)

**What they are:** Free tracking snippets. Since your orders come from Instagram
reels and videos, the **Meta Pixel** is what tells Facebook/Instagram which visitors
actually bought — without it you can't see which reel converts, can't retarget
people who abandoned carts, and can't run "optimize for purchases" ads.
GA4 (Google Analytics) is the general traffic dashboard.

**Cost:** **FREE**.

**Steps:**
1. **Meta Pixel:** business.facebook.com → Events Manager → Connect Data Sources →
   Web → create a Pixel → copy the **Pixel ID** (a number).
2. **GA4:** analytics.google.com → Admin → Create Property → Web →
   copy the **Measurement ID** (`G-XXXXXXX`).

**What goes in storefront `.env`:**
```
VITE_META_PIXEL_ID=1234567890
VITE_GA4_MEASUREMENT_ID=G-XXXXXXX
```
The full purchase funnel (view → add to cart → checkout → purchase) is already
wired — it activates the moment these IDs exist.

**Also add your real channel links** (shown in the home-page Follow Us section + footer):
```
VITE_INSTAGRAM_URL=...  VITE_YOUTUBE_URL=...  VITE_FACEBOOK_URL=...
```

---

## 13. Frontend Hosting (Storefront + Admin) 🔴

**What it is:** The two React apps are static files after build — host them free.

**Cost:** **FREE** (Vercel, Netlify, or Cloudflare Pages).

**Steps (Vercel):**
1. Sign up at vercel.com → import each project (or drag-drop the `dist` folder).
2. Set the storefront's env vars (all the `VITE_*` values) in Vercel's project
   settings — they're baked in at build time.
3. Point `yourstore.com` and `admin.yourstore.com` at Vercel (it shows you the DNS records).
4. Set `VITE_API_URL=https://api.yourstore.com/api/v1` so the apps call your backend.

---

## 14. Security Values You Generate Yourself 🔴

```
JWT_SECRET=          # run: openssl rand -base64 48   (NEVER change after launch — logs everyone out)
ADMIN_EMAIL=you@yourstore.com
ADMIN_PASSWORD=      # strong password; change again after first login
CORS_ORIGINS=https://yourstore.com,https://admin.yourstore.com
ENVIRONMENT=production
STORE_NAME=ElectroMart
STORE_URL=https://yourstore.com
```

---

## 15. Cost Summary

### One-time
| Item | Cost |
|---|---|
| DLT registration (SMS compliance) | ~₹5,900 |
| Domain (year 1) | ₹800–1,500 |
| GST registration (CA optional) | ₹0–3,000 |
| **Total one-time** | **~₹7,000–10,000** |

### Monthly fixed (launch configuration)
| Item | Cost |
|---|---|
| VPS (backend server) | ₹500–1,000 |
| MongoDB Atlas | FREE (M0) |
| Redis (Upstash) | FREE |
| Storefront + admin hosting | FREE |
| S3 images | ₹100–300 |
| Email (Zoho) | FREE |
| Pixel / GA4 / Google Sign-In | FREE |
| **Total fixed** | **~₹700–1,500/month** |

### Per-order variable
| Item | Cost |
|---|---|
| Stripe fee on the advance | ~2% + GST of amount charged |
| OTP SMS | ~₹0.20 per login |
| Shipping (500g surface) | ~₹40–90 |
| COD collection fee | ~₹40–50 or ~1.5–2% of COD amount |
| Order emails | ~free |

**Rule of thumb:** a ₹1,500 order (₹300 advance + ₹1,200 COD) costs you roughly
₹100–160 in fees+shipping. Price your products with that margin in mind.
At scale (10k visitors/day) fixed costs rise to ~₹8,000–12,000/month (bigger VPS +
Atlas M10). Your ad spend will likely exceed all infrastructure costs combined.

---

## 16. Recommended Order of Doing Things

1. **Week 1 (slow approvals — start first):** MSG91 DLT ⏰ · GST registration ⏰ · Stripe signup & activation ⏰
2. **Day 1–2 (fast):** Domain + Cloudflare · MongoDB Atlas · Upstash · AWS S3+IAM · Google OAuth · Zoho Mail · Meta Pixel · GA4
3. **Deploy:** VPS + backend `.env` · Vercel for both frontends
4. **Verify with test keys:** full purchase with Stripe test card `4242...` → invoice downloads → emails arrive → admin sees order
5. **First real shipment:** book via admin → we verify the Shipmozo field mapping together
6. **Flip to live keys** → place one real ₹1 order yourself → launch 🚀

---

*Generated 2026-07-26. Config reference: `.env.example` (backend) and
`ecom-store/.env.example` (storefront) always list every variable with comments.*
