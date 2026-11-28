# LucubraElec Operations Runbook

## Safety rules

- Treat production as read-only during investigation.
- Never retry a payment, refund, shipment, webhook, or inventory adjustment until the provider and MongoDB states have been compared.
- Never paste tokens, passwords, complete addresses, phone numbers, payment details, or raw provider payloads into tickets or logs.
- Use order IDs, provider reference IDs, event codes, and `X-Request-ID` as correlation keys.
- Take a database backup before a manual data correction. Record the approver, reason, previous value, new value, and verification evidence.

## First response

1. Record the environment, UTC timestamp, affected route, order number, and visible error.
2. Obtain `X-Request-ID` from the response or browser network panel.
3. Open Admin > Operations and search by order/resource ID.
4. Search Railway JSON logs for the request ID, event code, order ID, or provider reference.
5. Open the matching Sentry event and confirm release and environment.
6. Compare MongoDB state with the relevant provider before deciding on remediation.

## Health and deployment

- `/live` proves only that the process can answer HTTP.
- `/ready` proves MongoDB connectivity and required production configuration.
- Railway uses `/ready`; Docker uses `/live`.
- A failed `/ready` check must block promotion. Read its named checks; it never returns credential values.
- Startup failure for critical indexes or production configuration is intentional. Correct the data/configuration; do not disable the gate.

### Deployment failure

1. Read the first fatal startup line in Railway logs.
2. For configuration failures, set only the named missing variable and redeploy.
3. For duplicate-key index failures, export the duplicate records, determine the authoritative record, and obtain approval before cleanup.
4. Verify `/ready`, then one anonymous catalog request and one authenticated admin request.
5. Roll back the application revision if readiness does not recover. Do not weaken an index or authentication check to make deployment green.

## Payment incident

1. Locate the order and payment by order ID and provider order/payment reference.
2. Compare amount, currency, payment status, order status, and timestamps with Razorpay/Stripe/Cashfree.
3. If the provider shows success but MongoDB does not, do not ask the customer to pay again. Escalate for reconciliation.
4. If MongoDB shows paid but the provider does not, stop fulfillment and escalate.
5. Do not replay a payment webhook blindly. Verify its event ID and whether the transition already occurred.
6. After remediation, verify order state, inventory, receipt/invoice eligibility, and customer-visible status.

## Refund incident

1. Compare `refund_requested_amount`, `refund_amount`, `refund_id`, `payment_status`, and provider refund state.
2. A `refund_pending` anomaly requires provider verification before retry.
3. Never issue a second provider refund because the local update failed.
4. COD reimbursement is a separate manual obligation; do not mark it automatically refunded.
5. Verify the provider amount, MongoDB refund history, customer-visible status, and collected-revenue calculation.

## Shipping incident

1. Compare order shipment fields with the shipment collection and Shipmozo.
2. If Shipmozo has a shipment but MongoDB has none, do not create another shipment.
3. If MongoDB has a shipment without an AWB, investigate the provider reference and webhook history.
4. Never cancel a pushed order locally before Shipmozo reports it cancelled. Cancel it in Shipmozo first, then wait for provider confirmation before retrying the local cancellation.
5. Verify product weight and dimensions before booking or retrying.
6. After remediation, verify one shipment, one AWB, tracking URL, courier and customer-visible tracking.

## Inventory incident

1. Compare the immutable order-item snapshot with current product and variant stock.
2. Check whether `stock_released` is true for cancelled/expired orders.
3. Do not increment stock until order state, payment state and previous stock movement are proven.
4. For a correction, record SKU/product ID, previous quantity, delta, reason, approver and resulting quantity.

## Authentication incident

1. Confirm whether the failure is customer JWT, Google OAuth, phone OTP or admin JWT.
2. For Google, verify the exact browser origin and client ID configuration.
3. For 401 responses, confirm token expiry and environment; never copy the token into a ticket.
4. For admin login, verify the admin record and configured email. Reset credentials through the approved secret path, not a hardcoded password.

## Database incident

1. Check `/ready`, MongoDB connectivity and connection limits.
2. Prefer reads and exported evidence. Do not run unbounded collection scans during an incident.
3. Back up before any manual correction.
4. Verify critical indexes after recovery.
5. Confirm checkout, payment idempotency and shipment idempotency before reopening traffic.

## Operational event codes

- `OPS_PAYMENT_PENDING_STALE`: payment window exceeded.
- `OPS_PAYMENT_ORDER_MISMATCH`: provider/local order states may disagree.
- `OPS_PAYMENT_AMOUNT_MISMATCH`: provider amount, currency, or order identity differs from the stored payment; fulfillment is blocked pending reconciliation.
- `OPS_SHIPMENT_MISSING`: confirmed order has no shipment after the configured threshold.
- `OPS_SHIPMENT_AWB_MISSING`: shipment exists without an AWB.
- `OPS_SHIPMENT_CANCELLATION_MISMATCH`: local cancellation was blocked until Shipmozo cancellation is confirmed.
- `OPS_REFUND_PENDING_STALE`: refund needs provider comparison.
- `OPS_STOCK_ROLLBACK_INCOMPLETE`: cancelled/expired order needs inventory investigation.

## Closure evidence

Record the request ID, event code, order/resource ID, provider reference, confirmed customer impact, exact remediation, test evidence, deployment revision, and final customer/financial state. Do not record secrets or customer PII.
