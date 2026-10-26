package atlasmcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"govoice/server/mcp"
	"govoice/server/mcp/atlasmcp"
)

// Monday 12 October 2026, 08:00 UTC. The booking window therefore ends on 26 October.
var monday = time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)

const tomorrow = "2026-10-13"

type toolCaller func(tool string, args any) (map[string]any, error)

// start brings up one fixture server on a test clock and returns a caller that also decodes the
// structured result. Every test gets its own server, so ids always start from the seeded state.
func start(t *testing.T) (toolCaller, string) {
	t.Helper()
	return startWith(t, atlasmcp.Options{Token: "tok", Now: func() time.Time { return monday }})
}

func startWith(t *testing.T, o atlasmcp.Options) (toolCaller, string) {
	t.Helper()
	srv, err := atlasmcp.New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	c := mcp.NewSDKClient(mcp.AllowLoopbackHTTP())
	t.Cleanup(c.CloseIdleConnections)
	return func(tool string, args any) (map[string]any, error) {
		res, err := c.CallTool(context.Background(), ts.URL+"/mcp", "tok", tool, mustJSON(t, args))
		if err != nil {
			return nil, err
		}
		var out map[string]any
		if err := json.Unmarshal(res.JSON, &out); err != nil {
			return nil, err
		}
		return out, nil
	}, ts.URL
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return raw
}

func mustCall(t *testing.T, call toolCaller, tool string, args any) map[string]any {
	t.Helper()
	out, err := call(tool, args)
	if err != nil {
		t.Fatalf("%s(%v): %v", tool, args, err)
	}
	return out
}

func mustFail(t *testing.T, call toolCaller, tool string, args any) {
	t.Helper()
	if out, err := call(tool, args); err == nil {
		t.Fatalf("%s(%v) = %v, want an error", tool, args, out)
	}
}

func obj(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %v, want an object", key, m[key])
	}
	return v
}

func arr(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	v, ok := m[key].([]any)
	if !ok {
		t.Fatalf("%s = %v, want an array", key, m[key])
	}
	return v
}

func str(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("%s = %v, want a string", key, m[key])
	}
	return v
}

func num(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("%s = %v, want a number", key, m[key])
	}
	return v
}

func TestSeededFixtures(t *testing.T) {
	call, _ := start(t)

	for _, want := range []struct{ id, name, tier string }{
		{"t1", "Maria Lopez", "Gold"},
		{"t2", "James Okafor", "Silver"},
		{"t3", "Priya Nair", "Platinum"},
	} {
		out := mustCall(t, call, "get_traveller", map[string]any{"traveller_id": want.id})
		if out["found"] != true {
			t.Fatalf("get_traveller %s: found = %v", want.id, out["found"])
		}
		traveller := obj(t, out, "traveller")
		if got := str(t, traveller, "id"); got != want.id {
			t.Errorf("id = %q, want %q", got, want.id)
		}
		if got := str(t, traveller, "name"); got != want.name {
			t.Errorf("name = %q, want %q", got, want.name)
		}
		if got := str(t, traveller, "tier"); got != want.tier {
			t.Errorf("tier = %q, want %q", got, want.tier)
		}
	}

	// A name lookup ignores case and spacing; a miss is found:false, not an error.
	byName := mustCall(t, call, "get_traveller", map[string]any{"name": "  maria   LOPEZ "})
	if byName["found"] != true {
		t.Fatalf("case-insensitive name lookup: %v", byName)
	}
	if out := mustCall(t, call, "get_traveller", map[string]any{"name": "Nobody Here"}); out["found"] != false {
		t.Fatalf("unknown name: %v", out)
	}
	mustFail(t, call, "get_traveller", map[string]any{})

	// Each seeded traveller has exactly one confirmed future booking, with the exact id the
	// demo installer and tests pin.
	for _, want := range []struct{ traveller, booking string }{
		{"t1", "b1"}, {"t2", "b2"}, {"t3", "b3"},
	} {
		out := mustCall(t, call, "list_bookings", map[string]any{"traveller_id": want.traveller})
		bookings := arr(t, out, "bookings")
		if len(bookings) != 1 {
			t.Fatalf("%s bookings = %v, want one", want.traveller, bookings)
		}
		booking := bookings[0].(map[string]any)
		if got := str(t, booking, "id"); got != want.booking {
			t.Errorf("booking id = %q, want %q", got, want.booking)
		}
		if got := str(t, booking, "status"); got != "confirmed" {
			t.Errorf("booking status = %q", got)
		}
		if got := str(t, booking, "date"); got < tomorrow {
			t.Errorf("seeded booking date %q is not in the future", got)
		}
	}

	// Two payment methods each, and only the brand and last four digits are exposed.
	for _, id := range []string{"t1", "t2", "t3"} {
		out := mustCall(t, call, "list_payment_methods", map[string]any{"traveller_id": id})
		methods := arr(t, out, "payment_methods")
		if len(methods) != 2 {
			t.Fatalf("%s payment methods = %d, want 2", id, len(methods))
		}
		for _, raw := range methods {
			method := raw.(map[string]any)
			if _, ok := method["number"]; ok {
				t.Fatalf("payment method leaks a card number: %v", method)
			}
			if len(str(t, method, "last4")) != 4 {
				t.Errorf("last4 = %q", str(t, method, "last4"))
			}
		}
	}

	for _, want := range []struct {
		traveller, tier string
		points          float64
	}{
		{"t1", "Gold", 8250},
		{"t2", "Silver", 3400},
		{"t3", "Platinum", 21700},
	} {
		out := mustCall(t, call, "get_loyalty", map[string]any{"traveller_id": want.traveller})
		loyalty := obj(t, out, "loyalty")
		if got := str(t, loyalty, "tier"); got != want.tier {
			t.Errorf("%s tier = %q, want %q", want.traveller, got, want.tier)
		}
		if got := num(t, loyalty, "points"); got != want.points {
			t.Errorf("%s points = %v, want %v", want.traveller, got, want.points)
		}
	}
}

func TestSearchBookChangeCancelFlight(t *testing.T) {
	call, _ := start(t)

	search := mustCall(t, call, "search_flights", map[string]any{"from": "London", "to": "Paris"})
	if got := str(t, search, "date"); got != tomorrow {
		t.Fatalf("default date = %q, want %q", got, tomorrow)
	}
	flights := arr(t, search, "flights")
	if len(flights) != 3 {
		t.Fatalf("flights = %d, want 3", len(flights))
	}
	first := flights[0].(map[string]any)
	if got := str(t, first, "from"); got != "London" {
		t.Errorf("from = %q", got)
	}
	if got := str(t, first, "to"); got != "Paris" {
		t.Errorf("to = %q", got)
	}
	if got := str(t, first, "id"); !strings.HasPrefix(got, "fl-london-paris-"+tomorrow+"-") {
		t.Errorf("flight id = %q", got)
	}

	// The same query gives the same ids on the next call, so a search then a book agree.
	again := mustCall(t, call, "search_flights", map[string]any{"from": "london", "to": "PARIS"})
	if got := str(t, arr(t, again, "flights")[0].(map[string]any), "id"); got != str(t, first, "id") {
		t.Fatalf("flight id changed between searches: %q", got)
	}

	booked := mustCall(t, call, "book_flight", map[string]any{"traveller_id": "t1", "flight_id": str(t, first, "id")})
	booking := obj(t, booked, "booking")
	if got := str(t, booking, "id"); got != "b4" {
		t.Errorf("booking id = %q, want b4", got)
	}
	if got := str(t, booking, "kind"); got != "flight" {
		t.Errorf("kind = %q", got)
	}
	if got := str(t, booking, "status"); got != "confirmed" {
		t.Errorf("status = %q", got)
	}
	if got := num(t, booking, "amount_cents"); got != num(t, first, "price_cents") {
		t.Errorf("amount = %v, want the flight price %v", got, num(t, first, "price_cents"))
	}
	if str(t, booking, "reference") == "" {
		t.Error("booking has no supplier reference")
	}

	// Moving the booking to another day changes its day and price.
	later := mustCall(t, call, "search_flights", map[string]any{"from": "London", "to": "Paris", "date": "2026-10-16"})
	moved := arr(t, later, "flights")[1].(map[string]any)
	changed := mustCall(t, call, "change_flight", map[string]any{"booking_id": "b4", "flight_id": str(t, moved, "id")})
	updated := obj(t, changed, "booking")
	if got := str(t, updated, "date"); got != "2026-10-16" {
		t.Errorf("date after change = %q", got)
	}
	if got := num(t, updated, "amount_cents"); got != num(t, moved, "price_cents") {
		t.Errorf("amount after change = %v, want %v", got, num(t, moved, "price_cents"))
	}

	cancelled := mustCall(t, call, "cancel_booking", map[string]any{"booking_id": "b4"})
	if got := str(t, cancelled, "status"); got != "cancelled" {
		t.Errorf("cancel status = %q", got)
	}
	list := mustCall(t, call, "list_bookings", map[string]any{"traveller_id": "t1"})
	if got := len(arr(t, list, "bookings")); got != 1 {
		t.Fatalf("t1 bookings after cancel = %d, want only the seeded one", got)
	}
	// A cancelled booking cannot be cancelled or changed again.
	mustFail(t, call, "cancel_booking", map[string]any{"booking_id": "b4"})
	mustFail(t, call, "change_flight", map[string]any{"booking_id": "b4", "flight_id": str(t, moved, "id")})
}

func TestSearchBookStay(t *testing.T) {
	call, _ := start(t)

	search := mustCall(t, call, "search_stays", map[string]any{"city": "paris"})
	if got := str(t, search, "city"); got != "Paris" {
		t.Errorf("city = %q", got)
	}
	if got := str(t, search, "check_in"); got != tomorrow {
		t.Errorf("check_in = %q", got)
	}
	if got := str(t, search, "check_out"); got != "2026-10-15" {
		t.Errorf("check_out = %q", got)
	}
	if got := num(t, search, "nights"); got != 2 {
		t.Errorf("nights = %v", got)
	}
	stays := arr(t, search, "stays")
	if len(stays) != 2 {
		t.Fatalf("stays = %d, want 2", len(stays))
	}
	first := stays[0].(map[string]any)
	if got := str(t, first, "id"); !strings.HasPrefix(got, "st-paris-"+tomorrow+"-2026-10-15-") {
		t.Errorf("stay id = %q", got)
	}
	if got := num(t, first, "total_cents"); got != 2*num(t, first, "price_per_night_cents") {
		t.Errorf("total = %v, want two nights", got)
	}

	booked := mustCall(t, call, "book_stay", map[string]any{"traveller_id": "t2", "stay_id": str(t, first, "id")})
	booking := obj(t, booked, "booking")
	if got := str(t, booking, "kind"); got != "stay" {
		t.Errorf("kind = %q", got)
	}
	if got := str(t, booking, "city"); got != "Paris" {
		t.Errorf("city = %q", got)
	}
	if got := str(t, booking, "end_date"); got != "2026-10-15" {
		t.Errorf("end_date = %q", got)
	}
	if got := num(t, booking, "amount_cents"); got != num(t, first, "total_cents") {
		t.Errorf("amount = %v", got)
	}

	// Explicit dates pick the nights, and the last day of the window is still bookable.
	week := mustCall(t, call, "search_stays", map[string]any{"city": "Dubai", "check_in": "2026-10-20", "check_out": "2026-10-23"})
	if got := num(t, week, "nights"); got != 3 {
		t.Errorf("nights = %v, want 3", got)
	}
}

func TestSearchBookTransfer(t *testing.T) {
	call, _ := start(t)

	search := mustCall(t, call, "search_transfers", map[string]any{"from": "Dubai", "to": "Singapore"})
	if got := str(t, search, "date"); got != tomorrow {
		t.Errorf("date = %q", got)
	}
	transfers := arr(t, search, "transfers")
	if len(transfers) != 2 {
		t.Fatalf("transfers = %d, want 2", len(transfers))
	}
	first := transfers[0].(map[string]any)
	if got := str(t, first, "id"); !strings.HasPrefix(got, "tr-dubai-singapore-"+tomorrow+"-") {
		t.Errorf("transfer id = %q", got)
	}

	booked := mustCall(t, call, "book_transfer", map[string]any{"traveller_id": "t3", "transfer_id": str(t, first, "id")})
	booking := obj(t, booked, "booking")
	if got := str(t, booking, "kind"); got != "transfer" {
		t.Errorf("kind = %q", got)
	}
	if got := str(t, booking, "from"); got != "Dubai" {
		t.Errorf("from = %q", got)
	}
	if got := str(t, booking, "to"); got != "Singapore" {
		t.Errorf("to = %q", got)
	}
	if got := num(t, booking, "amount_cents"); got != num(t, first, "price_cents") {
		t.Errorf("amount = %v", got)
	}
}

func TestLoyaltyAndApplyPoints(t *testing.T) {
	call, _ := start(t)

	loyalty := obj(t, mustCall(t, call, "get_loyalty", map[string]any{"traveller_id": "t1"}), "loyalty")
	if got := str(t, loyalty, "next_tier"); got != "Platinum" {
		t.Errorf("next_tier = %q", got)
	}
	if got := num(t, loyalty, "points_to_next"); got != 6750 {
		t.Errorf("points_to_next = %v, want 6750", got)
	}

	// Redeeming enough points drops Maria from Gold to Silver, and the answer says so.
	applied := mustCall(t, call, "apply_points", map[string]any{"traveller_id": "t1", "points": 4000})
	if got := num(t, applied, "applied_points"); got != 4000 {
		t.Errorf("applied_points = %v", got)
	}
	downgraded := obj(t, applied, "loyalty")
	if got := num(t, downgraded, "points"); got != 4250 {
		t.Errorf("points = %v, want 4250", got)
	}
	if got := str(t, downgraded, "tier"); got != "Silver" {
		t.Errorf("tier = %q, want Silver", got)
	}
	if got := num(t, downgraded, "points_to_next"); got != 750 {
		t.Errorf("points_to_next = %v, want 750", got)
	}

	mustFail(t, call, "apply_points", map[string]any{"traveller_id": "t1", "points": 5000})
	mustFail(t, call, "apply_points", map[string]any{"traveller_id": "t1", "points": 0})
	mustFail(t, call, "apply_points", map[string]any{"traveller_id": "t1", "points": -10})
	mustFail(t, call, "apply_points", map[string]any{"traveller_id": "nobody", "points": 10})
}

func TestPaymentMethodsChargeAndRefund(t *testing.T) {
	call, _ := start(t)

	methods := arr(t, mustCall(t, call, "list_payment_methods", map[string]any{"traveller_id": "t1"}), "payment_methods")
	if got := len(methods); got != 2 {
		t.Fatalf("t1 payment methods = %d, want 2", got)
	}
	first := methods[0].(map[string]any)
	if got := str(t, first, "brand"); got != "Visa" {
		t.Errorf("brand = %q", got)
	}
	if first["default"] != true {
		t.Errorf("the first card is not the default: %v", first)
	}

	seeded := arr(t, mustCall(t, call, "list_bookings", map[string]any{"traveller_id": "t1"}), "bookings")[0].(map[string]any)
	charged := mustCall(t, call, "charge_booking", map[string]any{"booking_id": "b1", "payment_method_id": str(t, first, "id")})
	charge := obj(t, charged, "charge")
	if got := str(t, charge, "id"); got != "ch1" {
		t.Errorf("charge id = %q", got)
	}
	if got := str(t, charge, "status"); got != "captured" {
		t.Errorf("charge status = %q", got)
	}
	if got := num(t, charge, "amount_cents"); got != num(t, seeded, "amount_cents") {
		t.Errorf("charge amount = %v, want the booking amount", got)
	}
	// One charge per booking, and a card must belong to the booking's traveller.
	mustFail(t, call, "charge_booking", map[string]any{"booking_id": "b1", "payment_method_id": str(t, first, "id")})
	mustFail(t, call, "charge_booking", map[string]any{"booking_id": "b1", "payment_method_id": "pm3"})
	mustFail(t, call, "charge_booking", map[string]any{"booking_id": "b1", "payment_method_id": "nobody"})
	mustFail(t, call, "charge_booking", map[string]any{"booking_id": "b999", "payment_method_id": "pm1"})

	refunded := mustCall(t, call, "issue_refund", map[string]any{"charge_id": "ch1", "reason": "Flight cancelled"})
	refund := obj(t, refunded, "refund")
	if got := str(t, refund, "id"); got != "rf1" {
		t.Errorf("refund id = %q", got)
	}
	if got := str(t, refund, "status"); got != "refunded" {
		t.Errorf("refund status = %q", got)
	}
	if got := num(t, refund, "amount_cents"); got != num(t, charge, "amount_cents") {
		t.Errorf("refund amount = %v", got)
	}
	mustFail(t, call, "issue_refund", map[string]any{"charge_id": "ch1"})
	mustFail(t, call, "issue_refund", map[string]any{"charge_id": "ch999"})
}

func TestChargeRefusesCancelledBooking(t *testing.T) {
	call, _ := start(t)
	mustCall(t, call, "cancel_booking", map[string]any{"booking_id": "b1"})
	mustFail(t, call, "charge_booking", map[string]any{"booking_id": "b1", "payment_method_id": "pm1"})
}

func TestDisruptionAndRebook(t *testing.T) {
	call, _ := start(t)

	reported := mustCall(t, call, "report_disruption", map[string]any{"booking_id": "b1", "reason": "Airline cancelled the flight"})
	disruption := obj(t, reported, "disruption")
	if got := str(t, disruption, "id"); got != "d1" {
		t.Errorf("disruption id = %q", got)
	}
	if got := str(t, disruption, "status"); got != "reported" {
		t.Errorf("disruption status = %q", got)
	}
	if got := str(t, disruption, "booking_id"); got != "b1" {
		t.Errorf("booking_id = %q", got)
	}

	// The replacement is a new flight on the next day, and the old booking is closed as rebooked.
	rebooked := mustCall(t, call, "rebook_disruption", map[string]any{"disruption_id": "d1"})
	after := obj(t, rebooked, "disruption")
	if got := str(t, after, "status"); got != "rebooked" {
		t.Errorf("disruption status after rebook = %q", got)
	}
	if got := str(t, after, "new_booking_id"); got != "b4" {
		t.Errorf("new_booking_id = %q, want b4", got)
	}
	replacement := obj(t, rebooked, "booking")
	if got := str(t, replacement, "id"); got != "b4" {
		t.Errorf("replacement id = %q", got)
	}
	if got := str(t, replacement, "kind"); got != "flight" {
		t.Errorf("replacement kind = %q", got)
	}
	if got := str(t, replacement, "date"); got != tomorrow {
		t.Errorf("replacement date = %q, want %q", got, tomorrow)
	}
	if got := str(t, replacement, "from"); got != "London" {
		t.Errorf("replacement from = %q", got)
	}
	list := mustCall(t, call, "list_bookings", map[string]any{"traveller_id": "t1"})
	bookings := arr(t, list, "bookings")
	if len(bookings) != 1 {
		t.Fatalf("t1 bookings after rebook = %v, want only the replacement", bookings)
	}
	if got := str(t, bookings[0].(map[string]any), "id"); got != "b4" {
		t.Errorf("t1 booking id = %q, want b4", got)
	}

	mustFail(t, call, "rebook_disruption", map[string]any{"disruption_id": "d1"})
	mustFail(t, call, "rebook_disruption", map[string]any{"disruption_id": "d999"})
	mustFail(t, call, "report_disruption", map[string]any{"booking_id": "b1"})
	mustFail(t, call, "report_disruption", map[string]any{"booking_id": "b999"})
}

func TestRebookDisruptionHonoursDelay(t *testing.T) {
	call, _ := startWith(t, atlasmcp.Options{Token: "tok", Now: func() time.Time { return monday }, Delay: 40 * time.Millisecond})
	mustCall(t, call, "report_disruption", map[string]any{"booking_id": "b1"})
	began := time.Now()
	mustCall(t, call, "rebook_disruption", map[string]any{"disruption_id": "d1"})
	if elapsed := time.Since(began); elapsed < 30*time.Millisecond {
		t.Fatalf("rebook_disruption answered in %v, before the 40ms delay", elapsed)
	}
}

func TestRebookDisruptionStopsWhenCancelled(t *testing.T) {
	srv, err := atlasmcp.New(atlasmcp.Options{Token: "tok", Now: func() time.Time { return monday }, Delay: 400 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	c := mcp.NewSDKClient(mcp.AllowLoopbackHTTP())
	t.Cleanup(c.CloseIdleConnections)
	call := func(tool string, args any) (map[string]any, error) {
		res, err := c.CallTool(context.Background(), ts.URL+"/mcp", "tok", tool, mustJSON(t, args))
		if err != nil {
			return nil, err
		}
		var out map[string]any
		return out, json.Unmarshal(res.JSON, &out)
	}
	mustCall(t, call, "report_disruption", map[string]any{"booking_id": "b1"})

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := c.CallTool(ctx, ts.URL+"/mcp", "tok", "rebook_disruption", mustJSON(t, map[string]any{"disruption_id": "d1"})); err == nil {
		t.Fatal("a cancelled rebook_disruption call must fail")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("the cancelled call took %v", elapsed)
	}
	// The wait must stop with the call: no replacement booking is created after the delay ends.
	time.Sleep(600 * time.Millisecond)
	list := mustCall(t, call, "list_bookings", map[string]any{"traveller_id": "t1"})
	if got := len(arr(t, list, "bookings")); got != 1 {
		t.Fatalf("t1 bookings = %d after a cancelled rebook, want the seeded one only", got)
	}
}

func TestCreateCallback(t *testing.T) {
	call, _ := start(t)

	// A seeded traveller id resolves the name and phone on the server.
	byID := obj(t, mustCall(t, call, "create_callback", map[string]any{"traveller_id": "t1", "topic": "General enquiry"}), "callback")
	if got := str(t, byID, "id"); got != "cb1" {
		t.Errorf("callback id = %q", got)
	}
	if got := str(t, byID, "status"); got != "requested" {
		t.Errorf("status = %q", got)
	}
	if got := str(t, byID, "traveller_id"); got != "t1" {
		t.Errorf("traveller_id = %q", got)
	}
	if got := str(t, byID, "name"); got != "Maria Lopez" {
		t.Errorf("name = %q", got)
	}
	if got := str(t, byID, "phone"); got == "" {
		t.Error("callback has no phone")
	}
	if got := str(t, byID, "topic"); got != "General enquiry" {
		t.Errorf("topic = %q", got)
	}

	// The human-handoff step passes the caller's name and phone instead of an id.
	byPhone := obj(t, mustCall(t, call, "create_callback", map[string]any{"name": "Sam Taylor", "phone": "555 0199", "topic": "Lost baggage"}), "callback")
	if got := str(t, byPhone, "name"); got != "Sam Taylor" {
		t.Errorf("name = %q", got)
	}
	if got := str(t, byPhone, "phone"); got != "555 0199" {
		t.Errorf("phone = %q", got)
	}
	if got := str(t, byPhone, "topic"); got != "Lost baggage" {
		t.Errorf("topic = %q", got)
	}
	if id, ok := byPhone["traveller_id"]; ok && id != "" {
		t.Errorf("an unknown caller must not get a traveller id: %v", id)
	}

	// A name and phone that match a seeded traveller resolve back to that traveller.
	matched := obj(t, mustCall(t, call, "create_callback", map[string]any{"name": "Priya Nair", "phone": "555-0107", "topic": "Seat request"}), "callback")
	if got := str(t, matched, "traveller_id"); got != "t3" {
		t.Errorf("matched traveller_id = %q, want t3", got)
	}

	// The topic is required, and a caller must be identified either by id or by name and phone.
	mustFail(t, call, "create_callback", map[string]any{"traveller_id": "t1"})
	mustFail(t, call, "create_callback", map[string]any{"traveller_id": "nobody", "topic": "General enquiry"})
	mustFail(t, call, "create_callback", map[string]any{"name": "Sam Taylor", "topic": "Lost baggage"})
	mustFail(t, call, "create_callback", map[string]any{"phone": "555 0199", "topic": "Lost baggage"})
	mustFail(t, call, "create_callback", map[string]any{"topic": "Lost baggage"})
	mustFail(t, call, "create_callback", map[string]any{"traveller_id": "t1", "topic": strings.Repeat("x", 121)})

	// The queue is bounded: once it is full, the tool says so instead of growing without limit.
	created := 0
	for ; created < atlasmcp.MaxCallbacks; created++ {
		if _, err := call("create_callback", map[string]any{"traveller_id": "t1", "topic": "General enquiry"}); err != nil {
			break
		}
	}
	if created >= atlasmcp.MaxCallbacks {
		t.Fatalf("callback queue accepted %d callbacks without a bound", created)
	}
	mustFail(t, call, "create_callback", map[string]any{"traveller_id": "t1", "topic": "General enquiry"})
}

func TestBookingsAreBounded(t *testing.T) {
	call, _ := start(t)
	search := mustCall(t, call, "search_flights", map[string]any{"from": "London", "to": "Paris"})
	flightID := str(t, arr(t, search, "flights")[0].(map[string]any), "id")

	booked := 0
	for ; booked < atlasmcp.MaxBookings; booked++ {
		if _, err := call("book_flight", map[string]any{"traveller_id": "t1", "flight_id": flightID}); err != nil {
			break
		}
	}
	if booked >= atlasmcp.MaxBookings {
		t.Fatalf("booking list accepted %d bookings without a bound", booked)
	}
	if booked == 0 {
		t.Fatal("the booking bound must be reached only after some successful bookings")
	}
}

func TestInvalidArgumentsAreRejected(t *testing.T) {
	call, _ := start(t)
	cases := []struct {
		tool string
		args map[string]any
	}{
		{"get_traveller", map[string]any{}},
		{"list_bookings", map[string]any{"traveller_id": "t99"}},
		{"list_payment_methods", map[string]any{"traveller_id": "t99"}},
		{"search_flights", map[string]any{"from": "Berlin", "to": "Paris"}},
		{"search_flights", map[string]any{"from": "London", "to": "Atlantis"}},
		{"search_flights", map[string]any{"from": "London", "to": "London"}},
		{"search_flights", map[string]any{"from": "London", "to": "Paris", "date": "13/10/2026"}},
		{"search_flights", map[string]any{"from": "London", "to": "Paris", "date": "2026-10-11"}}, // yesterday
		{"search_flights", map[string]any{"from": "London", "to": "Paris", "date": "2026-10-27"}}, // past the window
		{"search_stays", map[string]any{"city": "Berlin"}},
		{"search_stays", map[string]any{"city": "Singapore", "check_in": "2026-10-26"}}, // no room for the default two nights
		{"search_stays", map[string]any{"city": "Paris", "check_in": "2026-10-20", "check_out": "2026-10-20"}},
		{"search_stays", map[string]any{"city": "Paris", "check_out": "2026-10-12"}}, // before the default check-in
		{"search_transfers", map[string]any{"from": "London", "to": "London"}},
		{"search_transfers", map[string]any{"from": "London", "to": "Paris", "date": "soon"}},
		{"book_flight", map[string]any{"traveller_id": "t99", "flight_id": "fl-london-paris-2026-10-13-1"}},
		{"book_flight", map[string]any{"traveller_id": "t1", "flight_id": "nonsense"}},
		{"book_flight", map[string]any{"traveller_id": "t1", "flight_id": "st-paris-2026-10-13-2026-10-15-1"}},
		{"book_stay", map[string]any{"traveller_id": "t1", "stay_id": "nonsense"}},
		{"book_transfer", map[string]any{"traveller_id": "t1", "transfer_id": "nonsense"}},
		{"change_flight", map[string]any{"booking_id": "b99", "flight_id": "fl-london-paris-2026-10-13-1"}},
		{"cancel_booking", map[string]any{"booking_id": "b99"}},
		{"charge_booking", map[string]any{"booking_id": "b99", "payment_method_id": "pm1"}},
		{"issue_refund", map[string]any{"charge_id": "ch99"}},
		{"report_disruption", map[string]any{"booking_id": "b99"}},
		{"rebook_disruption", map[string]any{"disruption_id": "d99"}},
		{"create_callback", map[string]any{"traveller_id": "t99"}},
	}
	for _, tc := range cases {
		if out, err := call(tc.tool, tc.args); err == nil {
			t.Errorf("%s(%v) = %v, want an error", tc.tool, tc.args, out)
		}
	}
}

func TestTokenIsRequiredOnMCPNotOnHealth(t *testing.T) {
	_, base := start(t)
	c := mcp.NewSDKClient(mcp.AllowLoopbackHTTP())
	defer c.CloseIdleConnections()
	if _, err := c.ListTools(context.Background(), base+"/mcp", "wrong"); !errors.Is(err, mcp.ErrAuthenticationFailed) {
		t.Fatalf("wrong token: %v", err)
	}
	resp, err := http.Get(base + "/mcp")
	if err != nil {
		t.Fatalf("GET /mcp: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /mcp without a token = %d, want 401", resp.StatusCode)
	}
	health, err := http.Get(base + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("/health = %d", health.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(health.Body).Decode(&body); err != nil || body["status"] != "ok" {
		t.Fatalf("/health body = %v err=%v", body, err)
	}
	if _, err := atlasmcp.New(atlasmcp.Options{}); !errors.Is(err, atlasmcp.ErrNoToken) {
		t.Fatalf("New without a token = %v, want ErrNoToken", err)
	}
}

// getJSON fetches a plain JSON endpoint and returns its status and decoded body.
func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("GET %s: decode: %v", url, err)
	}
	return resp.StatusCode, body
}

func TestWeatherEndpoint(t *testing.T) {
	_, base := start(t)

	status, body := getJSON(t, base+"/api/weather?city=london")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if got := str(t, body, "city"); got != "London" {
		t.Errorf("city = %q", got)
	}
	if got := str(t, body, "date"); got != tomorrow {
		t.Errorf("default date = %q, want %q", got, tomorrow)
	}
	if got := str(t, body, "condition"); got != "Light rain" {
		t.Errorf("condition = %q", got)
	}
	if got := num(t, body, "high_c"); got != 15 {
		t.Errorf("high_c = %v", got)
	}
	if got := num(t, body, "low_c"); got != 9 {
		t.Errorf("low_c = %v", got)
	}
	if got := num(t, body, "precipitation_chance"); got != 60 {
		t.Errorf("precipitation_chance = %v", got)
	}

	_, dated := getJSON(t, base+"/api/weather?city=Paris&date=2026-10-20")
	if got := str(t, dated, "date"); got != "2026-10-20" {
		t.Errorf("explicit date = %q", got)
	}
	if got := str(t, dated, "condition"); got != "Partly cloudy" {
		t.Errorf("condition = %q", got)
	}

	for _, query := range []string{
		"",                                      // no query
		"?date=2026-10-20",                      // no city
		"?city=Berlin",                          // unknown city
		"?city=London&date=yesterday",           // malformed date
		"?city=London&date=2026-10-27",          // past the window
		"?city=London&date=2026-10-12T00:00:00", // date with a time
	} {
		if status, body := getJSON(t, base+"/api/weather"+query); status != http.StatusBadRequest || str(t, body, "error") == "" {
			t.Errorf("GET /api/weather%s = %d %v, want 400 with an error", query, status, body)
		}
	}
}

func TestAdvisoryEndpoint(t *testing.T) {
	_, base := start(t)

	status, body := getJSON(t, base+"/api/advisory?country=France")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if got := str(t, body, "country"); got != "France" {
		t.Errorf("country = %q", got)
	}
	if got := str(t, body, "level"); got != "low" {
		t.Errorf("level = %q", got)
	}
	if got := str(t, body, "summary"); got == "" {
		t.Error("summary is empty")
	}
	if got := str(t, body, "updated"); got != "2026-10-12" {
		t.Errorf("updated = %q", got)
	}

	// A city or a short country form is accepted too.
	if _, alias := getJSON(t, base+"/api/advisory?country=paris"); str(t, alias, "country") != "France" {
		t.Errorf("city alias = %v", alias)
	}
	if _, alias := getJSON(t, base+"/api/advisory?country=UK"); str(t, alias, "country") != "United Kingdom" {
		t.Errorf("short country alias = %v", alias)
	}

	for _, query := range []string{"", "?country=Atlantis"} {
		if status, body := getJSON(t, base+"/api/advisory"+query); status != http.StatusBadRequest || str(t, body, "error") == "" {
			t.Errorf("GET /api/advisory%s = %d %v, want 400 with an error", query, status, body)
		}
	}
}

func TestExportedContract(t *testing.T) {
	wantToolNames := []string{
		"get_traveller", "list_bookings", "search_flights", "book_flight", "change_flight", "cancel_booking",
		"search_stays", "book_stay", "search_transfers", "book_transfer",
		"list_payment_methods", "charge_booking", "issue_refund",
		"get_loyalty", "apply_points",
		"report_disruption", "rebook_disruption", "create_callback",
	}
	if !slices.Equal(atlasmcp.ToolNames, wantToolNames) {
		t.Fatalf("ToolNames = %v", atlasmcp.ToolNames)
	}
	if len(wantToolNames) != 18 {
		t.Fatalf("the contract has %d tool names, want 18", len(wantToolNames))
	}

	if len(atlasmcp.HTTPTools) != 2 {
		t.Fatalf("HTTPTools = %d, want 2", len(atlasmcp.HTTPTools))
	}
	paths := map[string]string{"weather_forecast": "/api/weather", "travel_advisory": "/api/advisory"}
	required := map[string]string{"weather_forecast": "city", "travel_advisory": "country"}
	for _, tool := range atlasmcp.HTTPTools {
		if tool.Method != http.MethodGet {
			t.Errorf("%s method = %q, want GET", tool.Name, tool.Method)
		}
		if got := paths[tool.Name]; got == "" || tool.Path != got {
			t.Errorf("%s path = %q, want %q", tool.Name, tool.Path, got)
		}
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
		if err := mcp.CheckInputSchema(context.Background(), tool.InputSchema); err != nil {
			t.Errorf("%s input schema: %v", tool.Name, err)
			continue
		}
		var schema struct {
			Type                 string                     `json:"type"`
			Required             []string                   `json:"required"`
			AdditionalProperties *bool                      `json:"additionalProperties"`
			Properties           map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Errorf("%s schema: %v", tool.Name, err)
			continue
		}
		if schema.Type != "object" {
			t.Errorf("%s schema type = %q, want object", tool.Name, schema.Type)
		}
		if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			t.Errorf("%s schema must set additionalProperties false", tool.Name)
		}
		if !slices.Contains(schema.Required, required[tool.Name]) {
			t.Errorf("%s required = %v, want %q", tool.Name, schema.Required, required[tool.Name])
		}
		if _, ok := schema.Properties[required[tool.Name]]; !ok {
			t.Errorf("%s schema has no %q property", tool.Name, required[tool.Name])
		}
	}

	// The MCP endpoint lists exactly the contract names, each with a usable schema.
	_, base := start(t)
	c := mcp.NewSDKClient(mcp.AllowLoopbackHTTP())
	defer c.CloseIdleConnections()
	tools, err := c.ListTools(context.Background(), base+"/mcp", "tok")
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var gotNames []string
	for _, tool := range tools {
		gotNames = append(gotNames, tool.Name)
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
		if err := mcp.CheckInputSchema(context.Background(), tool.InputSchema); err != nil {
			t.Errorf("%s input schema: %v", tool.Name, err)
		}
	}
	slices.Sort(gotNames)
	sorted := slices.Clone(atlasmcp.ToolNames)
	slices.Sort(sorted)
	if !slices.Equal(gotNames, sorted) {
		t.Fatalf("registered tools = %v, want %v", gotNames, sorted)
	}
}

// The replacement path has a branch per booking kind: a stay keeps its length
// and a transfer keeps its endpoints.
func TestRebookAStayAndATransfer(t *testing.T) {
	call, _ := start(t)

	// b3 is the seeded two-night stay in Singapore.
	reported := mustCall(t, call, "report_disruption", map[string]any{"booking_id": "b3", "reason": "Hotel closed"})
	stayDisruption := str(t, obj(t, reported, "disruption"), "id")
	rebooked := mustCall(t, call, "rebook_disruption", map[string]any{"disruption_id": stayDisruption})
	stay := obj(t, rebooked, "booking")
	if got := str(t, stay, "kind"); got != "stay" {
		t.Errorf("stay replacement kind = %q", got)
	}
	if got := str(t, stay, "date"); got != tomorrow {
		t.Errorf("stay replacement date = %q, want %q", got, tomorrow)
	}
	if got := str(t, stay, "end_date"); got != "2026-10-15" {
		t.Errorf("stay replacement end_date = %q, want 2026-10-15 (two nights)", got)
	}

	// A transfer booking rebooks as another transfer, same endpoints.
	found := mustCall(t, call, "search_transfers", map[string]any{"from": "London", "to": "Paris"})
	transfer := arr(t, found, "transfers")[0].(map[string]any)
	booked := mustCall(t, call, "book_transfer", map[string]any{"traveller_id": "t1", "transfer_id": str(t, transfer, "id")})
	transferBooking := str(t, obj(t, booked, "booking"), "id")
	reported = mustCall(t, call, "report_disruption", map[string]any{"booking_id": transferBooking})
	transferDisruption := str(t, obj(t, reported, "disruption"), "id")
	rebooked = mustCall(t, call, "rebook_disruption", map[string]any{"disruption_id": transferDisruption})
	replacement := obj(t, rebooked, "booking")
	if got := str(t, replacement, "kind"); got != "transfer" {
		t.Errorf("transfer replacement kind = %q", got)
	}
	if got := str(t, replacement, "from"); got != "London" {
		t.Errorf("transfer replacement from = %q", got)
	}

	// change_flight only accepts a flight booking.
	flights := mustCall(t, call, "search_flights", map[string]any{"from": "London", "to": "Paris"})
	flightID := str(t, arr(t, flights, "flights")[0].(map[string]any), "id")
	mustFail(t, call, "change_flight", map[string]any{"booking_id": "b3", "flight_id": flightID})
}

// Two callers rebooking one disruption must not create two replacement bookings.
func TestConcurrentRebookOfOneDisruption(t *testing.T) {
	call, _ := start(t)
	reported := mustCall(t, call, "report_disruption", map[string]any{"booking_id": "b1"})
	id := str(t, obj(t, reported, "disruption"), "id")

	var wg sync.WaitGroup
	var successes atomic.Int32
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := call("rebook_disruption", map[string]any{"disruption_id": id}); err == nil {
				successes.Add(1)
			} else {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	if successes.Load() != 1 {
		t.Fatalf("successful rebooks = %d, want 1", successes.Load())
	}
	list := mustCall(t, call, "list_bookings", map[string]any{"traveller_id": "t1"})
	if bookings := arr(t, list, "bookings"); len(bookings) != 1 {
		t.Fatalf("t1 bookings after concurrent rebook = %d, want only the replacement", len(bookings))
	}
}
