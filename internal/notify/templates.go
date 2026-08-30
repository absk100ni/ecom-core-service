package notify

// All HTML email templates as Go constants. Inline CSS, mobile-friendly.

const tplOrderConfirmed = `<!DOCTYPE html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="margin:0;padding:0;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;background:#f5f5f5">
<div style="max-width:600px;margin:20px auto;background:#fff;border-radius:8px;overflow:hidden;box-shadow:0 2px 8px rgba(0,0,0,.08)">
<div style="background:#2c3e50;color:#fff;padding:24px;text-align:center">
<h1 style="margin:0;font-size:22px">%s</h1>
<p style="margin:8px 0 0;opacity:0.9">Order Confirmed ✓</p>
</div>
<div style="padding:24px">
<p style="margin:0 0 16px;font-size:16px;color:#333">Your order <strong>%s</strong> has been placed successfully!</p>
<table style="width:100%%;border-collapse:collapse;margin:16px 0">
<thead><tr style="background:#f8f9fa"><th style="padding:8px;text-align:left">Item</th><th style="padding:8px;text-align:center">Qty</th><th style="padding:8px;text-align:right">Amount</th></tr></thead>
<tbody>%s</tbody>
<tfoot><tr style="background:#f8f9fa;font-weight:bold"><td colspan="2" style="padding:8px">Total</td><td style="padding:8px;text-align:right">₹%.2f</td></tr></tfoot>
</table>
%s%s
<p style="margin:10px 0;color:#555"><strong>Shipping to:</strong> %s</p>
<div style="text-align:center;margin:24px 0">
<a href="%s/orders/%s" style="display:inline-block;padding:12px 32px;background:#3498db;color:#fff;text-decoration:none;border-radius:6px;font-weight:bold">View Order</a>
</div>
</div>
<div style="background:#f8f9fa;padding:16px;text-align:center;color:#888;font-size:12px">
<p style="margin:0">© %s. All rights reserved.</p>
</div>
</div></body></html>`

const tplOrderShipped = `<!DOCTYPE html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="margin:0;padding:0;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;background:#f5f5f5">
<div style="max-width:600px;margin:20px auto;background:#fff;border-radius:8px;overflow:hidden;box-shadow:0 2px 8px rgba(0,0,0,.08)">
<div style="background:#27ae60;color:#fff;padding:24px;text-align:center">
<h1 style="margin:0;font-size:22px">%s</h1>
<p style="margin:8px 0 0;opacity:0.9">Order Shipped 🚚</p>
</div>
<div style="padding:24px">
<p style="margin:0 0 16px;font-size:16px;color:#333">Your order <strong>%s</strong> is on its way!</p>
<div style="background:#f0fff4;border:1px solid #c3e6cb;border-radius:6px;padding:16px;margin:16px 0">
<p style="margin:0 0 8px"><strong>Courier:</strong> %s</p>
<p style="margin:0 0 8px"><strong>Tracking ID:</strong> %s</p>
<p style="margin:0"><strong>Track:</strong> <a href="%s" style="color:#3498db">%s</a></p>
</div>
%s
</div>
<div style="background:#f8f9fa;padding:16px;text-align:center;color:#888;font-size:12px">
<p style="margin:0">© %s. All rights reserved.</p>
</div>
</div></body></html>`

const tplRefundProcessed = `<!DOCTYPE html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="margin:0;padding:0;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;background:#f5f5f5">
<div style="max-width:600px;margin:20px auto;background:#fff;border-radius:8px;overflow:hidden;box-shadow:0 2px 8px rgba(0,0,0,.08)">
<div style="background:#8e44ad;color:#fff;padding:24px;text-align:center">
<h1 style="margin:0;font-size:22px">%s</h1>
<p style="margin:8px 0 0;opacity:0.9">Refund Processed 💸</p>
</div>
<div style="padding:24px">
<p style="margin:0 0 16px;font-size:16px;color:#333">Your refund for order <strong>%s</strong> has been processed.</p>
<div style="background:#f8f0ff;border:1px solid #d6bcfa;border-radius:6px;padding:16px;margin:16px 0;text-align:center">
<p style="margin:0;font-size:24px;font-weight:bold;color:#8e44ad">₹%.2f</p>
<p style="margin:8px 0 0;color:#666">will be credited to your original payment method within 5-7 business days.</p>
</div>
</div>
<div style="background:#f8f9fa;padding:16px;text-align:center;color:#888;font-size:12px">
<p style="margin:0">© %s. All rights reserved.</p>
</div>
</div></body></html>`

const tplContactNotify = `<!DOCTYPE html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"></head>
<body style="margin:0;padding:0;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;background:#f5f5f5">
<div style="max-width:600px;margin:20px auto;background:#fff;border-radius:8px;overflow:hidden;box-shadow:0 2px 8px rgba(0,0,0,.08)">
<div style="background:#e74c3c;color:#fff;padding:24px;text-align:center">
<h1 style="margin:0;font-size:22px">%s — New Contact Message</h1>
</div>
<div style="padding:24px">
<p><strong>From:</strong> %s (%s)</p>
<p><strong>Subject:</strong> %s</p>
<div style="background:#f8f9fa;border-radius:6px;padding:16px;margin:16px 0;white-space:pre-wrap">%s</div>
</div>
<div style="background:#f8f9fa;padding:16px;text-align:center;color:#888;font-size:12px">
<p style="margin:0">© %s Admin Panel</p>
</div>
</div></body></html>`
