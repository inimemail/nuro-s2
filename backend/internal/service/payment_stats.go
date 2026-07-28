package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentauditlog"
	"github.com/Wei-Shaw/sub2api/ent/paymentorder"
)

// --- Dashboard & Analytics ---

func (s *PaymentService) GetDashboardStats(ctx context.Context, days int) (*DashboardStats, error) {
	if days <= 0 {
		days = 30
	}
	now := time.Now()
	since := now.AddDate(0, 0, -days)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	paidStatuses := []string{OrderStatusCompleted, OrderStatusPaid, OrderStatusRecharging}

	orders, err := s.entClient.PaymentOrder.Query().
		Where(
			paymentorder.StatusIn(paidStatuses...),
			paymentorder.PaidAtGTE(since),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}

	st := &DashboardStats{}
	st.PrimaryCurrency, st.Currencies = paymentDashboardCurrencies(orders)
	computeBasicStats(st, orders, todayStart)

	st.PendingOrders, err = s.entClient.PaymentOrder.Query().
		Where(paymentorder.StatusEQ(OrderStatusPending)).
		Count(ctx)
	if err != nil {
		return nil, err
	}

	st.DailySeries = buildDailySeries(orders, since, days)
	st.PaymentMethods = buildMethodDistribution(orders)
	st.TopUsersByCurrency = buildTopUsersByCurrency(orders)
	st.TopUsers = st.TopUsersByCurrency[st.PrimaryCurrency]
	for i := range st.DailySeries {
		st.DailySeries[i].Amount = st.DailySeries[i].AmountByCurrency[st.PrimaryCurrency]
	}
	for i := range st.PaymentMethods {
		st.PaymentMethods[i].Amount = st.PaymentMethods[i].AmountByCurrency[st.PrimaryCurrency]
	}

	return st, nil
}

func computeBasicStats(st *DashboardStats, orders []*dbent.PaymentOrder, todayStart time.Time) {
	st.TotalAmountByCurrency = make(CurrencyAmounts)
	st.TodayAmountByCurrency = make(CurrencyAmounts)
	st.AvgAmountByCurrency = make(CurrencyAmounts)
	currencyCounts := make(map[string]int)
	var todayCount int
	for _, o := range orders {
		currency := PaymentOrderCurrency(o)
		st.TotalAmountByCurrency[currency] += o.PayAmount
		currencyCounts[currency]++
		if o.PaidAt != nil && !o.PaidAt.Before(todayStart) {
			st.TodayAmountByCurrency[currency] += o.PayAmount
			todayCount++
		}
	}
	st.TotalCount = len(orders)
	st.TodayCount = todayCount
	for currency, amount := range st.TotalAmountByCurrency {
		st.AvgAmountByCurrency[currency] = roundPaymentDashboardAmount(amount / float64(currencyCounts[currency]))
	}
	roundPaymentDashboardCurrencyAmounts(st.TotalAmountByCurrency)
	roundPaymentDashboardCurrencyAmounts(st.TodayAmountByCurrency)
	st.TotalAmount = st.TotalAmountByCurrency[st.PrimaryCurrency]
	st.TodayAmount = st.TodayAmountByCurrency[st.PrimaryCurrency]
	st.AvgAmount = st.AvgAmountByCurrency[st.PrimaryCurrency]
}

func buildDailySeries(orders []*dbent.PaymentOrder, since time.Time, days int) []DailyStats {
	dailyMap := make(map[string]*DailyStats)
	for _, o := range orders {
		if o.PaidAt == nil {
			continue
		}
		date := o.PaidAt.Format("2006-01-02")
		ds, ok := dailyMap[date]
		if !ok {
			ds = &DailyStats{Date: date, AmountByCurrency: make(CurrencyAmounts)}
			dailyMap[date] = ds
		}
		ds.AmountByCurrency[PaymentOrderCurrency(o)] += o.PayAmount
		ds.Count++
	}
	series := make([]DailyStats, 0, days)
	for i := 0; i < days; i++ {
		date := since.AddDate(0, 0, i+1).Format("2006-01-02")
		if ds, ok := dailyMap[date]; ok {
			roundPaymentDashboardCurrencyAmounts(ds.AmountByCurrency)
			series = append(series, *ds)
		} else {
			series = append(series, DailyStats{Date: date, AmountByCurrency: make(CurrencyAmounts)})
		}
	}
	return series
}

func buildMethodDistribution(orders []*dbent.PaymentOrder) []PaymentMethodStat {
	methodMap := make(map[string]*PaymentMethodStat)
	for _, o := range orders {
		ms, ok := methodMap[o.PaymentType]
		if !ok {
			ms = &PaymentMethodStat{Type: o.PaymentType, AmountByCurrency: make(CurrencyAmounts)}
			methodMap[o.PaymentType] = ms
		}
		ms.AmountByCurrency[PaymentOrderCurrency(o)] += o.PayAmount
		ms.Count++
	}
	methods := make([]PaymentMethodStat, 0, len(methodMap))
	for _, ms := range methodMap {
		roundPaymentDashboardCurrencyAmounts(ms.AmountByCurrency)
		methods = append(methods, *ms)
	}
	sort.Slice(methods, func(i, j int) bool { return methods[i].Type < methods[j].Type })
	return methods
}

func buildTopUsersByCurrency(orders []*dbent.PaymentOrder) TopUsersByCurrency {
	userMap := make(map[string]map[int64]*TopUserStat)
	for _, o := range orders {
		currency := PaymentOrderCurrency(o)
		users := userMap[currency]
		if users == nil {
			users = make(map[int64]*TopUserStat)
			userMap[currency] = users
		}
		us, ok := users[o.UserID]
		if !ok {
			us = &TopUserStat{UserID: o.UserID, Email: o.UserEmail}
			users[o.UserID] = us
		}
		us.Amount += o.PayAmount
	}
	result := make(TopUsersByCurrency, len(userMap))
	for currency, users := range userMap {
		userList := make([]*TopUserStat, 0, len(users))
		for _, us := range users {
			us.Amount = roundPaymentDashboardAmount(us.Amount)
			userList = append(userList, us)
		}
		sort.Slice(userList, func(i, j int) bool { return userList[i].Amount > userList[j].Amount })
		limit := topUsersLimit
		if len(userList) < limit {
			limit = len(userList)
		}
		result[currency] = make([]TopUserStat, 0, limit)
		for i := 0; i < limit; i++ {
			result[currency] = append(result[currency], *userList[i])
		}
	}
	return result
}

func buildTopUsers(orders []*dbent.PaymentOrder) []TopUserStat {
	primary, _ := paymentDashboardCurrencies(orders)
	return buildTopUsersByCurrency(orders)[primary]
}

func paymentDashboardCurrencies(orders []*dbent.PaymentOrder) (string, []string) {
	seen := make(map[string]struct{})
	for _, order := range orders {
		seen[PaymentOrderCurrency(order)] = struct{}{}
	}
	currencies := make([]string, 0, len(seen))
	for currency := range seen {
		currencies = append(currencies, currency)
	}
	sort.Strings(currencies)
	primary := "CNY"
	if _, ok := seen[primary]; !ok {
		if len(currencies) > 0 {
			primary = currencies[0]
		} else {
			primary = "CNY"
		}
	}
	return primary, currencies
}

func roundPaymentDashboardCurrencyAmounts(amounts CurrencyAmounts) {
	for currency, amount := range amounts {
		amounts[currency] = roundPaymentDashboardAmount(amount)
	}
}

func roundPaymentDashboardAmount(amount float64) float64 {
	return math.Round(amount*100) / 100
}

// --- Audit Logs ---

func (s *PaymentService) writeAuditLog(ctx context.Context, oid int64, action, op string, detail map[string]any) {
	dj, _ := json.Marshal(detail)
	_, err := s.entClient.PaymentAuditLog.Create().SetOrderID(strconv.FormatInt(oid, 10)).SetAction(action).SetDetail(string(dj)).SetOperator(op).Save(ctx)
	if err != nil {
		slog.Error("audit log failed", "orderID", oid, "action", action, "error", err)
	}
}

func (s *PaymentService) GetOrderAuditLogs(ctx context.Context, oid int64) ([]*dbent.PaymentAuditLog, error) {
	return s.entClient.PaymentAuditLog.Query().Where(paymentauditlog.OrderIDEQ(strconv.FormatInt(oid, 10))).Order(paymentauditlog.ByCreatedAt()).All(ctx)
}
