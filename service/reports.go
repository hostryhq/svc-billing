package service

import (
	"context"
	"sort"

	"github.com/hostryhq/go-common/apperr"

	"github.com/hostryhq/svc-billing/internal/gen"
	"github.com/hostryhq/svc-billing/repository"
)

// Reports read the stored aggregates (agg.go), never the raw bills, so they stay cheap and a report cannot disagree with
// the day totals. Each report belongs wholly to billing (ADR-0014 R7) and needs the internet (RM-R5).

// Range is the date range of a report; either end may be empty.
type Range struct{ From, To string }

// Report is a keyed list: rows sorted for display, each with a stable key for the cursor.
type Report[T any] struct {
	OutletID string
	Items    []T
	Next     string
	Totals   map[string]int64
}

// reportOutlet resolves the one outlet a report covers: the selected one, or the caller's only outlet.
func (s *Service) reportOutlet(ctx context.Context, selected string) (string, error) {
	sc, err := s.Scope(ctx, "report.view.outlet", selected)
	if err != nil {
		return "", err
	}
	if len(sc) != 1 {
		return "", validation("outlet_required", "select an outlet with X-Outlet-Id", field("X-Outlet-Id", "required"))
	}
	return sc[0], nil
}

func checkRange(r Range) error {
	if r.From != "" && r.To != "" && r.From > r.To {
		return apperr.New(apperr.Validation, "validation", "from is after to").WithFields(field("from", "after_to"))
	}
	return nil
}

func keyPage[T any](rows []T, key func(T) string, after string, limit int) ([]T, string) {
	start := 0
	if after != "" {
		for i, r := range rows {
			if key(r) == after {
				start = i + 1
				break
			}
		}
	}
	rows = rows[start:]
	if len(rows) > limit {
		return rows[:limit], key(rows[limit-1])
	}
	return rows, ""
}

func (s *Service) aggDocs(ctx context.Context, kind, outlet string, r Range) ([]repository.Doc, error) {
	var docs []repository.Doc
	err := s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		var err error
		docs, err = tx.FindDocs(ctx, repository.Query{Kind: kind, Outlets: []string{outlet}, DateFrom: r.From, DateTo: r.To, Asc: true})
		return err
	})
	return docs, err
}

func decodeAll[T any](docs []repository.Doc) ([]T, error) {
	out := make([]T, 0, len(docs))
	for _, d := range docs {
		var v T
		if err := jsonUnmarshal(d.Body, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// DaySummary is report 1: one row per business day.
func (s *Service) DaySummary(ctx context.Context, selected string, r Range, after string, limit int) (Report[gen.DaySummaryRow], error) {
	outlet, err := s.reportOutlet(ctx, selected)
	if err != nil {
		return Report[gen.DaySummaryRow]{}, err
	}
	if err := checkRange(r); err != nil {
		return Report[gen.DaySummaryRow]{}, err
	}
	docs, err := s.aggDocs(ctx, repository.KindAggDay, outlet, r)
	if err != nil {
		return Report[gen.DaySummaryRow]{}, err
	}
	var rows []gen.DaySummaryRow
	err = s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		for _, d := range docs {
			var t gen.DayTotals
			if err := jsonUnmarshal(d.Body, &t); err != nil {
				return err
			}
			row := gen.DaySummaryRow{BusinessDate: mustDate(d.Date), Status: gen.DaySummaryRowStatusOpen, Totals: t}
			days, _, err := loadAll[gen.BusinessDay](ctx, tx, repository.Query{Kind: repository.KindBusinessDay, Outlets: []string{outlet}, DateFrom: d.Date, DateTo: d.Date})
			if err != nil {
				return err
			}
			if len(days) > 0 {
				row.Status, row.RevisionCount = gen.DaySummaryRowStatus(days[0].Status), days[0].RevisionCount
			}
			rows = append(rows, row)
		}
		return nil
	})
	if err != nil {
		return Report[gen.DaySummaryRow]{}, err
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].BusinessDate.String() > rows[j].BusinessDate.String() })
	items, next := keyPage(rows, func(x gen.DaySummaryRow) string { return x.BusinessDate.String() }, after, limit)
	return Report[gen.DaySummaryRow]{OutletID: outlet, Items: items, Next: next}, nil
}

// TaxSummary is report 3: by invoice date, bills and credit notes apart; revisions never touch it.
func (s *Service) TaxSummary(ctx context.Context, selected string, r Range, after string, limit int) (Report[gen.TaxSummaryRow], error) {
	outlet, err := s.reportOutlet(ctx, selected)
	if err != nil {
		return Report[gen.TaxSummaryRow]{}, err
	}
	if err := checkRange(r); err != nil {
		return Report[gen.TaxSummaryRow]{}, err
	}
	docs, err := s.aggDocs(ctx, repository.KindAggTax, outlet, r)
	if err != nil {
		return Report[gen.TaxSummaryRow]{}, err
	}
	aggs, err := decodeAll[AggTax](docs)
	if err != nil {
		return Report[gen.TaxSummaryRow]{}, err
	}
	by := map[string]*gen.TaxSummaryRow{}
	var nonGST, taxable, cgst, sgst int64
	for _, a := range aggs {
		k := a.Row.TaxClass + "|" + itoa(a.Row.RateBp)
		row := by[k]
		if row == nil {
			c := a.Row
			z := money(0)
			c.BillTaxable, c.BillCgst, c.BillSgst, c.CreditTaxable, c.CreditCgst, c.CreditSgst = z, z, z, z, z, z
			row = &c
			by[k] = row
		}
		addTo(&row.BillTaxable, a.Row.BillTaxable.AmountPaise)
		addTo(&row.BillCgst, a.Row.BillCgst.AmountPaise)
		addTo(&row.BillSgst, a.Row.BillSgst.AmountPaise)
		addTo(&row.CreditTaxable, a.Row.CreditTaxable.AmountPaise)
		addTo(&row.CreditCgst, a.Row.CreditCgst.AmountPaise)
		addTo(&row.CreditSgst, a.Row.CreditSgst.AmountPaise)
		nonGST += a.NonGST
		taxable += a.Row.BillTaxable.AmountPaise - a.Row.CreditTaxable.AmountPaise
		cgst += a.Row.BillCgst.AmountPaise - a.Row.CreditCgst.AmountPaise
		sgst += a.Row.BillSgst.AmountPaise - a.Row.CreditSgst.AmountPaise
	}
	rows := make([]gen.TaxSummaryRow, 0, len(by))
	for _, v := range by {
		rows = append(rows, *v)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].TaxClass != rows[j].TaxClass {
			return rows[i].TaxClass < rows[j].TaxClass
		}
		return rows[i].RateBp < rows[j].RateBp
	})
	items, next := keyPage(rows, func(x gen.TaxSummaryRow) string { return x.TaxClass + "|" + itoa(x.RateBp) }, after, limit)
	return Report[gen.TaxSummaryRow]{OutletID: outlet, Items: items, Next: next,
		Totals: map[string]int64{"net_taxable": taxable, "net_cgst": cgst, "net_sgst": sgst, "non_gst": nonGST}}, nil
}

// TenderSummary is report 4: cash and UPI are never combined; other tenders by name.
func (s *Service) TenderSummary(ctx context.Context, selected string, r Range, after string, limit int) (Report[gen.TenderSummaryRow], error) {
	outlet, err := s.reportOutlet(ctx, selected)
	if err != nil {
		return Report[gen.TenderSummaryRow]{}, err
	}
	if err := checkRange(r); err != nil {
		return Report[gen.TenderSummaryRow]{}, err
	}
	docs, err := s.aggDocs(ctx, repository.KindAggTender, outlet, r)
	if err != nil {
		return Report[gen.TenderSummaryRow]{}, err
	}
	aggs, err := decodeAll[gen.TenderSummaryRow](docs)
	if err != nil {
		return Report[gen.TenderSummaryRow]{}, err
	}
	by := map[string]*gen.TenderSummaryRow{}
	for _, a := range aggs {
		k := string(a.Tender) + "|" + deref(a.Name)
		row := by[k]
		if row == nil {
			c := a
			c.PaymentCount, c.Paid, c.Refunded, c.Net = 0, money(0), money(0), money(0)
			row = &c
			by[k] = row
		}
		row.PaymentCount += a.PaymentCount
		addTo(&row.Paid, a.Paid.AmountPaise)
		addTo(&row.Refunded, a.Refunded.AmountPaise)
		row.Net = money(row.Paid.AmountPaise - row.Refunded.AmountPaise)
	}
	rows := make([]gen.TenderSummaryRow, 0, len(by))
	for _, v := range by {
		rows = append(rows, *v)
	}
	sort.Slice(rows, func(i, j int) bool {
		return string(rows[i].Tender)+deref(rows[i].Name) < string(rows[j].Tender)+deref(rows[j].Name)
	})
	items, next := keyPage(rows, func(x gen.TenderSummaryRow) string { return string(x.Tender) + "|" + deref(x.Name) }, after, limit)
	return Report[gen.TenderSummaryRow]{OutletID: outlet, Items: items, Next: next}, nil
}

// ItemSales is report 5, variant level, top first.
func (s *Service) ItemSales(ctx context.Context, selected string, r Range, after string, limit int) (Report[gen.ItemSalesRow], error) {
	outlet, err := s.reportOutlet(ctx, selected)
	if err != nil {
		return Report[gen.ItemSalesRow]{}, err
	}
	if err := checkRange(r); err != nil {
		return Report[gen.ItemSalesRow]{}, err
	}
	docs, err := s.aggDocs(ctx, repository.KindAggItem, outlet, r)
	if err != nil {
		return Report[gen.ItemSalesRow]{}, err
	}
	aggs, err := decodeAll[gen.ItemSalesRow](docs)
	if err != nil {
		return Report[gen.ItemSalesRow]{}, err
	}
	by := map[string]*gen.ItemSalesRow{}
	for _, a := range aggs {
		k := a.ItemId + "|" + deref(a.VariantName)
		if by[k] == nil {
			c := a
			c.QtyMilli, c.Gross, c.Net = 0, money(0), money(0)
			by[k] = &c
		}
		by[k].QtyMilli += a.QtyMilli
		addTo(&by[k].Gross, a.Gross.AmountPaise)
		addTo(&by[k].Net, a.Net.AmountPaise)
	}
	rows := make([]gen.ItemSalesRow, 0, len(by))
	for _, v := range by {
		rows = append(rows, *v)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Net.AmountPaise != rows[j].Net.AmountPaise {
			return rows[i].Net.AmountPaise > rows[j].Net.AmountPaise
		}
		return rows[i].ItemId+deref(rows[i].VariantName) < rows[j].ItemId+deref(rows[j].VariantName)
	})
	items, next := keyPage(rows, func(x gen.ItemSalesRow) string { return x.ItemId + "|" + deref(x.VariantName) }, after, limit)
	return Report[gen.ItemSalesRow]{OutletID: outlet, Items: items, Next: next}, nil
}

// CategorySales is report 6.
func (s *Service) CategorySales(ctx context.Context, selected string, r Range, after string, limit int) (Report[gen.CategorySalesRow], error) {
	outlet, err := s.reportOutlet(ctx, selected)
	if err != nil {
		return Report[gen.CategorySalesRow]{}, err
	}
	if err := checkRange(r); err != nil {
		return Report[gen.CategorySalesRow]{}, err
	}
	docs, err := s.aggDocs(ctx, repository.KindAggCategory, outlet, r)
	if err != nil {
		return Report[gen.CategorySalesRow]{}, err
	}
	aggs, err := decodeAll[gen.CategorySalesRow](docs)
	if err != nil {
		return Report[gen.CategorySalesRow]{}, err
	}
	by := map[string]*gen.CategorySalesRow{}
	for _, a := range aggs {
		k := a.CategoryId + "|" + a.CategoryName
		if by[k] == nil {
			c := a
			c.QtyMilli, c.Gross, c.Net = 0, money(0), money(0)
			by[k] = &c
		}
		by[k].QtyMilli += a.QtyMilli
		addTo(&by[k].Gross, a.Gross.AmountPaise)
		addTo(&by[k].Net, a.Net.AmountPaise)
	}
	rows := make([]gen.CategorySalesRow, 0, len(by))
	for _, v := range by {
		rows = append(rows, *v)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Net.AmountPaise != rows[j].Net.AmountPaise {
			return rows[i].Net.AmountPaise > rows[j].Net.AmountPaise
		}
		return rows[i].CategoryId < rows[j].CategoryId
	})
	items, next := keyPage(rows, func(x gen.CategorySalesRow) string { return x.CategoryId + "|" + x.CategoryName }, after, limit)
	return Report[gen.CategorySalesRow]{OutletID: outlet, Items: items, Next: next}, nil
}

// StaffSales is report 8: by cashier and by waiter.
func (s *Service) StaffSales(ctx context.Context, selected string, r Range, after string, limit int) (Report[gen.StaffSalesRow], error) {
	outlet, err := s.reportOutlet(ctx, selected)
	if err != nil {
		return Report[gen.StaffSalesRow]{}, err
	}
	if err := checkRange(r); err != nil {
		return Report[gen.StaffSalesRow]{}, err
	}
	docs, err := s.aggDocs(ctx, repository.KindAggStaff, outlet, r)
	if err != nil {
		return Report[gen.StaffSalesRow]{}, err
	}
	aggs, err := decodeAll[gen.StaffSalesRow](docs)
	if err != nil {
		return Report[gen.StaffSalesRow]{}, err
	}
	by := map[string]*gen.StaffSalesRow{}
	for _, a := range aggs {
		k := a.MemberId + "|" + string(a.Role)
		if by[k] == nil {
			c := a
			c.BillCount, c.Net = 0, money(0)
			by[k] = &c
		}
		by[k].BillCount += a.BillCount
		addTo(&by[k].Net, a.Net.AmountPaise)
	}
	rows := make([]gen.StaffSalesRow, 0, len(by))
	for _, v := range by {
		rows = append(rows, *v)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Net.AmountPaise != rows[j].Net.AmountPaise {
			return rows[i].Net.AmountPaise > rows[j].Net.AmountPaise
		}
		return rows[i].MemberId+string(rows[i].Role) < rows[j].MemberId+string(rows[j].Role)
	})
	items, next := keyPage(rows, func(x gen.StaffSalesRow) string { return x.MemberId + "|" + string(x.Role) }, after, limit)
	return Report[gen.StaffSalesRow]{OutletID: outlet, Items: items, Next: next}, nil
}

// HourlySales is report 10: order type and hour.
func (s *Service) HourlySales(ctx context.Context, selected string, r Range, after string, limit int) (Report[gen.HourlySalesRow], error) {
	outlet, err := s.reportOutlet(ctx, selected)
	if err != nil {
		return Report[gen.HourlySalesRow]{}, err
	}
	if err := checkRange(r); err != nil {
		return Report[gen.HourlySalesRow]{}, err
	}
	docs, err := s.aggDocs(ctx, repository.KindAggHourly, outlet, r)
	if err != nil {
		return Report[gen.HourlySalesRow]{}, err
	}
	rows, err := decodeAll[gen.HourlySalesRow](docs)
	if err != nil {
		return Report[gen.HourlySalesRow]{}, err
	}
	key := func(x gen.HourlySalesRow) string {
		return x.BusinessDate.String() + "|" + itoa(100+x.Hour) + "|" + string(x.OrderType)
	}
	sort.Slice(rows, func(i, j int) bool { return key(rows[i]) < key(rows[j]) })
	items, next := keyPage(rows, key, after, limit)
	return Report[gen.HourlySalesRow]{OutletID: outlet, Items: items, Next: next}, nil
}

// Discounts is report 7, billing part: discounts, comps, refunds, credit notes and internal closures.
func (s *Service) Discounts(ctx context.Context, selected string, r Range, kind, after string, limit int) (Report[gen.DiscountRow], error) {
	outlet, err := s.reportOutlet(ctx, selected)
	if err != nil {
		return Report[gen.DiscountRow]{}, err
	}
	if err := checkRange(r); err != nil {
		return Report[gen.DiscountRow]{}, err
	}
	var rows []gen.DiscountRow
	err = s.repo.InTx(ctx, func(ctx context.Context, tx repository.Tx) error {
		docs, err := tx.FindDocs(ctx, repository.Query{Kind: repository.KindDiscount, Outlets: []string{outlet}, DateFrom: r.From, DateTo: r.To, K1: kind})
		if err != nil {
			return err
		}
		rows, err = decodeAll[gen.DiscountRow](docs)
		return err
	})
	if err != nil {
		return Report[gen.DiscountRow]{}, err
	}
	var total int64
	for _, x := range rows {
		total += x.Value.AmountPaise
	}
	key := func(x gen.DiscountRow) string { return x.At.String() + "|" + string(x.Kind) + "|" + x.SubjectId }
	sort.Slice(rows, func(i, j int) bool { return key(rows[i]) > key(rows[j]) })
	items, next := keyPage(rows, key, after, limit)
	return Report[gen.DiscountRow]{OutletID: outlet, Items: items, Next: next, Totals: map[string]int64{"count": int64(len(rows)), "value": total}}, nil
}
