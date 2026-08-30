package invoice

import (
	"fmt"
	"strings"
	"time"

	"ecom-core-service/internal/models"
)

func (h *Handler) renderInvoiceHTML(order *models.Order, lines []InvoiceLine, isIntra bool) string {
	var sb strings.Builder

	invDate := time.Now()
	if order.InvoiceDate != nil {
		invDate = *order.InvoiceDate
	}

	addr := order.ShippingAddress
	buyerAddr := fmt.Sprintf("%s<br>%s", addr.Name, addr.Line1)
	if addr.Line2 != "" {
		buyerAddr += "<br>" + addr.Line2
	}
	buyerAddr += fmt.Sprintf("<br>%s, %s - %s", addr.City, addr.State, addr.Pincode)

	// Header
	sb.WriteString(`<!DOCTYPE html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">`)
	sb.WriteString(`<title>Invoice ` + order.InvoiceNumber + `</title>`)
	sb.WriteString(`<style>
@media print{body{margin:0}@page{size:A4;margin:10mm}}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;font-size:12px;color:#333;max-width:210mm;margin:0 auto;padding:20px}
.inv-header{display:flex;justify-content:space-between;border-bottom:2px solid #333;padding-bottom:12px;margin-bottom:16px}
.inv-title{font-size:24px;font-weight:bold}
table{width:100%;border-collapse:collapse;margin:12px 0}
th,td{padding:6px 8px;border:1px solid #ddd;text-align:left}
th{background:#f5f5f5;font-weight:600}
.text-right{text-align:right}
.text-center{text-align:center}
.totals{margin-top:16px;display:flex;justify-content:flex-end}
.totals table{width:auto;min-width:250px}
.addr-block{display:flex;gap:40px;margin:12px 0}
.addr-block div{flex:1}
.footer{margin-top:30px;border-top:1px solid #ddd;padding-top:12px;font-size:11px;color:#666}
</style></head><body>`)

	// Invoice header
	sb.WriteString(`<div class="inv-header"><div>`)
	sb.WriteString(fmt.Sprintf(`<div class="inv-title">TAX INVOICE</div>`))
	sb.WriteString(fmt.Sprintf(`<div><strong>%s</strong></div>`, h.cfg.BusinessLegalName))
	sb.WriteString(fmt.Sprintf(`<div>%s</div>`, h.cfg.BusinessAddress))
	sb.WriteString(fmt.Sprintf(`<div>GSTIN: %s</div>`, h.cfg.BusinessGSTIN))
	sb.WriteString(fmt.Sprintf(`<div>State: %s (%s)</div>`, h.cfg.BusinessState, h.cfg.BusinessStateCode))
	sb.WriteString(`</div><div style="text-align:right">`)
	sb.WriteString(fmt.Sprintf(`<div><strong>Invoice #:</strong> %s</div>`, order.InvoiceNumber))
	sb.WriteString(fmt.Sprintf(`<div><strong>Date:</strong> %s</div>`, invDate.Format("02-Jan-2006")))
	sb.WriteString(fmt.Sprintf(`<div><strong>Order:</strong> %s</div>`, order.OrderNumber))
	sb.WriteString(`</div></div>`)

	// Addresses
	sb.WriteString(`<div class="addr-block">`)
	sb.WriteString(fmt.Sprintf(`<div><strong>Bill To / Ship To:</strong><br>%s<br>Phone: %s</div>`, buyerAddr, addr.Phone))
	sb.WriteString(fmt.Sprintf(`<div><strong>Place of Supply:</strong> %s</div>`, addr.State))
	sb.WriteString(`</div>`)

	// Line items table
	if isIntra {
		sb.WriteString(`<table><thead><tr><th>#</th><th>Description</th><th>HSN</th><th class="text-center">Qty</th><th class="text-right">Unit Price</th><th class="text-right">Taxable</th><th class="text-center">CGST %</th><th class="text-right">CGST</th><th class="text-center">SGST %</th><th class="text-right">SGST</th><th class="text-right">Total</th></tr></thead><tbody>`)
	} else {
		sb.WriteString(`<table><thead><tr><th>#</th><th>Description</th><th>HSN</th><th class="text-center">Qty</th><th class="text-right">Unit Price</th><th class="text-right">Taxable</th><th class="text-center">IGST %</th><th class="text-right">IGST</th><th class="text-right">Total</th></tr></thead><tbody>`)
	}

	var totalTaxable, totalGST, grandTotal int
	for i, line := range lines {
		totalTaxable += line.TaxableValue
		totalGST += line.GSTAmount
		grandTotal += line.LineTotal
		if isIntra {
			halfRate := line.GSTRate / 2
			halfTax := line.GSTAmount / 2
			// Handle odd paise: CGST gets floor, SGST gets remainder
			sgstTax := line.GSTAmount - halfTax
			sb.WriteString(fmt.Sprintf(`<tr><td>%d</td><td>%s</td><td>%s</td><td class="text-center">%d</td><td class="text-right">₹%.2f</td><td class="text-right">₹%.2f</td><td class="text-center">%d%%</td><td class="text-right">₹%.2f</td><td class="text-center">%d%%</td><td class="text-right">₹%.2f</td><td class="text-right">₹%.2f</td></tr>`,
				i+1, line.Description, line.HSN, line.Qty,
				float64(line.UnitPrice)/100, float64(line.TaxableValue)/100,
				halfRate, float64(halfTax)/100,
				halfRate, float64(sgstTax)/100,
				float64(line.LineTotal)/100))
		} else {
			sb.WriteString(fmt.Sprintf(`<tr><td>%d</td><td>%s</td><td>%s</td><td class="text-center">%d</td><td class="text-right">₹%.2f</td><td class="text-right">₹%.2f</td><td class="text-center">%d%%</td><td class="text-right">₹%.2f</td><td class="text-right">₹%.2f</td></tr>`,
				i+1, line.Description, line.HSN, line.Qty,
				float64(line.UnitPrice)/100, float64(line.TaxableValue)/100,
				line.GSTRate, float64(line.GSTAmount)/100,
				float64(line.LineTotal)/100))
		}
	}
	sb.WriteString(`</tbody></table>`)

	// Totals
	sb.WriteString(`<div class="totals"><table>`)
	sb.WriteString(fmt.Sprintf(`<tr><td>Taxable Amount</td><td class="text-right">₹%.2f</td></tr>`, float64(totalTaxable)/100))
	if isIntra {
		sb.WriteString(fmt.Sprintf(`<tr><td>CGST</td><td class="text-right">₹%.2f</td></tr>`, float64(totalGST/2)/100))
		sb.WriteString(fmt.Sprintf(`<tr><td>SGST</td><td class="text-right">₹%.2f</td></tr>`, float64(totalGST-totalGST/2)/100))
	} else {
		sb.WriteString(fmt.Sprintf(`<tr><td>IGST</td><td class="text-right">₹%.2f</td></tr>`, float64(totalGST)/100))
	}
	if order.ShippingCost > 0 {
		sb.WriteString(fmt.Sprintf(`<tr><td>Shipping</td><td class="text-right">₹%.2f</td></tr>`, float64(order.ShippingCost)/100))
		grandTotal += order.ShippingCost
	}
	if order.Discount > 0 {
		sb.WriteString(fmt.Sprintf(`<tr><td>Discount</td><td class="text-right">-₹%.2f</td></tr>`, float64(order.Discount)/100))
		grandTotal -= order.Discount
	}
	sb.WriteString(fmt.Sprintf(`<tr style="font-weight:bold;border-top:2px solid #333"><td>Grand Total</td><td class="text-right">₹%.2f</td></tr>`, float64(grandTotal)/100))
	sb.WriteString(`</table></div>`)

	// Footer
	sb.WriteString(`<div class="footer">`)
	sb.WriteString(`<p>This is a computer-generated invoice and does not require a signature.</p>`)
	sb.WriteString(fmt.Sprintf(`<p>%s | GSTIN: %s</p>`, h.cfg.BusinessLegalName, h.cfg.BusinessGSTIN))
	sb.WriteString(`</div></body></html>`)

	return sb.String()
}
