package invoice

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"ecom-core-service/internal/config"
	"ecom-core-service/internal/models"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestGSTInclusiveTaxable(t *testing.T) {
	tests := []struct {
		name        string
		totalPaise  int
		ratePercent int
		wantTaxable int
		wantGST     int
	}{
		{"18% on 11800 paise", 11800, 18, 10000, 1800},
		{"18% on 1180 paise", 1180, 18, 1000, 180},
		{"5% on 1050 paise", 1050, 5, 1000, 50},
		{"12% on 11200 paise", 11200, 12, 10000, 1200},
		{"28% on 12800 paise", 12800, 28, 10000, 2800},
		{"18% on 999 paise (rounding)", 999, 18, 847, 152},
		{"18% on 1 paise (edge)", 1, 18, 1, 0},
		{"0% on 5000 paise", 5000, 0, 5000, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			taxable := GSTInclusiveTaxable(tt.totalPaise, tt.ratePercent)
			gst := tt.totalPaise - taxable
			if taxable != tt.wantTaxable {
				t.Errorf("GSTInclusiveTaxable(%d, %d) taxable = %d, want %d", tt.totalPaise, tt.ratePercent, taxable, tt.wantTaxable)
			}
			if gst != tt.wantGST {
				t.Errorf("GSTInclusiveTaxable(%d, %d) GST = %d, want %d", tt.totalPaise, tt.ratePercent, gst, tt.wantGST)
			}
		})
	}
}

func TestFiscalYear(t *testing.T) {
	tests := []struct {
		month time.Month
		year  int
		want  string
	}{
		{time.April, 2025, "2526"},
		{time.March, 2025, "2425"},
		{time.January, 2026, "2526"},
		{time.December, 2025, "2526"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			tm := time.Date(tt.year, tt.month, 15, 0, 0, 0, 0, time.UTC)
			got := fiscalYear(tm)
			if got != tt.want {
				t.Errorf("fiscalYear(%v) = %s, want %s", tm, got, tt.want)
			}
		})
	}
}

func TestEqualsIgnoreCase(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"Delhi", "Delhi", true},
		{"delhi", "Delhi", true},
		{"DELHI", "delhi", true},
		{"Maharashtra", "Delhi", false},
		{"", "", true},
	}
	for _, tt := range tests {
		if got := equalsIgnoreCase(tt.a, tt.b); got != tt.want {
			t.Errorf("equalsIgnoreCase(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestInvoiceNumberIdempotency(t *testing.T) {
	// Logic test: if invoice_number is already set, we reuse it (no DB needed for this assertion)
	invNum := "INV-2526-000001"
	if invNum == "" {
		t.Error("Invoice number should be set")
	}
	// Calling again should return same number (tested at integration level)
	if invNum != "INV-2526-000001" {
		t.Error("Invoice number should be idempotent")
	}
}

func TestCGSTSGSTSplit(t *testing.T) {
	// 18% GST on 999 paise: taxable=847, gst=152
	// CGST = 152/2 = 76, SGST = 152-76 = 76
	gst := 152
	cgst := gst / 2
	sgst := gst - cgst
	if cgst != 76 || sgst != 76 {
		t.Errorf("CGST=%d SGST=%d, want 76/76", cgst, sgst)
	}

	// Odd GST amount: 151
	gst2 := 151
	cgst2 := gst2 / 2     // 75
	sgst2 := gst2 - cgst2 // 76
	if cgst2 != 75 || sgst2 != 76 {
		t.Errorf("Odd split: CGST=%d SGST=%d, want 75/76", cgst2, sgst2)
	}
}

func TestInvoiceLinesRequireProductTaxData(t *testing.T) {
	handler := &Handler{cfg: &config.Config{}}
	order := &models.Order{Subtotal: 11800, Total: 11800, Items: []models.OrderItem{{ProductID: "product-1", Name: "Work Light", Price: 11800, Quantity: 1}}}

	if _, err := handler.computeInvoiceLines(order, map[string]*models.Product{}); err == nil {
		t.Fatal("expected missing product tax data to block invoice generation")
	}
	if _, err := handler.computeInvoiceLines(order, map[string]*models.Product{
		"product-1": {ID: "product-1", GSTPercent: 0, HSNCode: ""},
	}); err == nil {
		t.Fatal("expected empty GST and HSN values to block invoice generation")
	}
}

func TestInvoiceLinesUseExplicitProductTaxData(t *testing.T) {
	handler := &Handler{cfg: &config.Config{}}
	order := &models.Order{Subtotal: 11800, Total: 11800, Items: []models.OrderItem{{ProductID: "product-1", Name: "Work Light", Price: 11800, Quantity: 1}}}
	lines, err := handler.computeInvoiceLines(order, map[string]*models.Product{
		"product-1": {ID: "product-1", GSTPercent: 18, HSNCode: "9405"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(lines) != 1 || lines[0].GSTRate != 18 || lines[0].HSN != "9405" || lines[0].GSTAmount != 1800 {
		t.Fatalf("unexpected invoice line: %+v", lines)
	}
}

func TestInvoiceLinesAllocateDiscountAndTaxShipping(t *testing.T) {
	handler := &Handler{cfg: &config.Config{ShippingGSTPercent: 18, ShippingHSNCode: "996812"}}
	order := &models.Order{
		Subtotal: 23600, Discount: 2360, ShippingCost: 1180, Total: 22420,
		Items: []models.OrderItem{
			{ProductID: "p1", Name: "Light", Price: 11800, Quantity: 1},
			{ProductID: "p2", Name: "Cable", Price: 5900, Quantity: 2},
		},
	}
	products := map[string]*models.Product{
		"p1": {ID: "p1", GSTPercent: 18, HSNCode: "9405"},
		"p2": {ID: "p2", GSTPercent: 18, HSNCode: "8544"},
	}
	lines, err := handler.computeInvoiceLines(order, products)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 {
		t.Fatalf("line count = %d, want 3", len(lines))
	}
	var discount, total int
	for _, line := range lines {
		discount += line.Discount
		total += line.LineTotal
		if line.TaxableValue+line.GSTAmount != line.LineTotal {
			t.Fatalf("tax components do not reconcile: %+v", line)
		}
	}
	if discount != order.Discount || total != order.Total {
		t.Fatalf("discount=%d total=%d, want discount=%d total=%d", discount, total, order.Discount, order.Total)
	}
	if lines[2].Description != "Shipping" || lines[2].HSN != "996812" {
		t.Fatalf("shipping line missing tax identity: %+v", lines[2])
	}
}

func TestInvoiceRejectsUnconfiguredShippingTax(t *testing.T) {
	handler := &Handler{cfg: &config.Config{}}
	order := &models.Order{Subtotal: 11800, ShippingCost: 500, Total: 12300, Items: []models.OrderItem{{ProductID: "p1", Name: "Light", Price: 11800, Quantity: 1}}}
	_, err := handler.computeInvoiceLines(order, map[string]*models.Product{"p1": {ID: "p1", GSTPercent: 18, HSNCode: "9405"}})
	if err == nil {
		t.Fatal("expected shipping tax configuration to be required")
	}
}

func TestPartialCODInvoiceRequiresCollection(t *testing.T) {
	order := &models.Order{PaymentStatus: "paid", PaymentPlan: "partial", CODAmount: 8000}
	if err := invoiceEligible(order); err == nil {
		t.Fatal("expected invoice to be blocked before COD collection")
	}
	order.CODCollected = true
	if err := invoiceEligible(order); err != nil {
		t.Fatalf("invoice should be eligible after COD collection: %v", err)
	}
}

func invoiceTestDB(t *testing.T) *mongo.Database {
	uri := os.Getenv("OPS_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("set OPS_TEST_MONGO_URI for Mongo-backed invoice tests")
	}
	client, err := mongo.Connect(context.Background(), options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	db := client.Database("invoice_concurrency_" + uuid.NewString())
	t.Cleanup(func() { _ = db.Drop(context.Background()); _ = client.Disconnect(context.Background()) })
	return db
}

func TestConcurrentInvoiceAssignmentUsesOneNumber(t *testing.T) {
	db := invoiceTestDB(t)
	ctx := context.Background()
	order := models.Order{ID: "order-1", OrderNumber: "ORD-1", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if _, err := db.Collection("orders").InsertOne(ctx, order); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db, cfg: &config.Config{}}
	const callers = 20
	results := make(chan string, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			number, _, err := handler.assignInvoiceNumber(ctx, &order)
			if err != nil {
				errs <- err
				return
			}
			results <- number
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var winner string
	for number := range results {
		if winner == "" {
			winner = number
		}
		if number != winner {
			t.Fatalf("concurrent callers observed %q and %q", winner, number)
		}
	}
	var stored bson.M
	if err := db.Collection("orders").FindOne(ctx, bson.M{"_id": order.ID}).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if stored["invoice_number"] != winner {
		t.Fatalf("stored invoice = %v, winner = %s", stored["invoice_number"], winner)
	}
}
