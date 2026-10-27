// Package atlasmcp is the travel fixture backend of the Atlas Travel Concierge demo: a small
// MCP server over streamable HTTP with fixed fake travellers, bookings, inventory, payments,
// disruptions and callbacks, plus two plain JSON endpoints for the demo's HTTP tools.
//
// It holds no real data and keeps nothing across restarts. Fixtures are seeded relative to the
// injected clock, every slice is bounded, and every tool validates its arguments so a model can
// recover from a bad id by calling a search tool again.
package atlasmcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// MaxBookings and MaxCallbacks bound the fixtures a tool can grow; the rest
	// are internal bounds because no tool creates a traveller, charge or disruption
	// beyond one per booking.
	MaxBookings  = 400
	MaxCallbacks = 200

	maxRefunds     = 200
	maxCharges     = 400
	maxDisruptions = 200

	// horizonDays is how far ahead the fixtures accept a date; today counts as day zero.
	horizonDays = 14
	// maxField caps every free-text and identifier field the fixtures accept.
	maxField = 120
	// dateLayout is the only accepted date spelling.
	dateLayout = "2006-01-02"
	// currency is the one currency the fixtures quote prices in.
	currency = "USD"

	kindFlight   = "flight"
	kindStay     = "stay"
	kindTransfer = "transfer"

	statusConfirmed = "confirmed"
	statusCancelled = "cancelled"
	statusRebooked  = "rebooked"

	disruptionReported = "reported"
	disruptionRebooked = "rebooked"
)

var (
	// ToolNames are the remote names of the server's MCP tools, in registration order.
	ToolNames = []string{
		"get_traveller", "list_bookings", "search_flights", "book_flight", "change_flight", "cancel_booking",
		"search_stays", "book_stay", "search_transfers", "book_transfer",
		"list_payment_methods", "charge_booking", "issue_refund",
		"get_loyalty", "apply_points",
		"report_disruption", "rebook_disruption", "create_callback",
	}

	// HTTPTools are the two plain JSON endpoints the demo exposes as HTTP tools. InputSchema is
	// the object schema handed to the HTTP tool definition; each scalar argument becomes a query
	// parameter, because both endpoints answer GET.
	HTTPTools = []HTTPTool{
		{
			Name:        "weather_forecast",
			Description: "Get the forecast for one Atlas city (London, Paris, New York, Dubai or Singapore) on a day within the next 14 days. date is YYYY-MM-DD and defaults to tomorrow.",
			Method:      http.MethodGet,
			Path:        "/api/weather",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"city": {"type": "string", "description": "London, Paris, New York, Dubai or Singapore."},
					"date": {"type": "string", "description": "Forecast day as YYYY-MM-DD; defaults to tomorrow."}
				},
				"required": ["city"],
				"additionalProperties": false
			}`),
		},
		{
			Name:        "travel_advisory",
			Description: "Get the travel advisory level and a short summary for a country the demo serves (United Kingdom, France, United States, United Arab Emirates or Singapore). A served city name is accepted too.",
			Method:      http.MethodGet,
			Path:        "/api/advisory",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"country": {"type": "string", "description": "United Kingdom, France, United States, United Arab Emirates or Singapore; a city name such as Paris is accepted too."}
				},
				"required": ["country"],
				"additionalProperties": false
			}`),
		},
	}

	// ErrNoToken is returned when Options.Token is empty; /mcp refuses every request without it.
	ErrNoToken = errors.New("atlasmcp: a bearer token is required")
)

// Options configures the fixture server.
type Options struct {
	Token string           // bearer token /mcp expects (required)
	Now   func() time.Time // the clock; the default is time.Now, read in UTC
	// Delay is how long rebook_disruption works before it answers, so the async tool behaves
	// like a real back-office rebooking. Zero answers at once; cmd/atlas-mcp passes 2s (tests 0).
	Delay time.Duration
}

// HTTPTool is one plain HTTP endpoint exposed as an HTTP tool definition by the demo installer.
type HTTPTool struct {
	Name        string
	Description string
	Method      string
	Path        string
	InputSchema json.RawMessage
}

// Traveller is one fake caller profile. Points is the loyalty balance; tier is kept in step with it.
type Traveller struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Tier   string `json:"tier"`
	Email  string `json:"email"`
	Phone  string `json:"phone"`
	Points int    `json:"-"`
}

// Flight is one searchable flight option. Its id is the handle book_flight and change_flight take.
type Flight struct {
	ID         string `json:"id"`
	From       string `json:"from"`
	To         string `json:"to"`
	Date       string `json:"date"`
	Depart     string `json:"depart"`
	Arrive     string `json:"arrive"`
	Airline    string `json:"airline"`
	Stops      int    `json:"stops"`
	PriceCents int    `json:"price_cents"`
	Currency   string `json:"currency"`
}

// Stay is one searchable hotel option. Its id is the handle book_stay takes.
type Stay struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	City               string  `json:"city"`
	CheckIn            string  `json:"check_in"`
	CheckOut           string  `json:"check_out"`
	Nights             int     `json:"nights"`
	PricePerNightCents int     `json:"price_per_night_cents"`
	TotalCents         int     `json:"total_cents"`
	Rating             float64 `json:"rating"`
	Currency           string  `json:"currency"`
}

// Transfer is one searchable ground option. Its id is the handle book_transfer takes.
type Transfer struct {
	ID         string `json:"id"`
	From       string `json:"from"`
	To         string `json:"to"`
	Date       string `json:"date"`
	Vehicle    string `json:"vehicle"`
	PriceCents int    `json:"price_cents"`
	Currency   string `json:"currency"`
}

// Booking is one confirmed, cancelled or rebooked travel booking.
type Booking struct {
	ID          string `json:"id"`
	TravellerID string `json:"traveller_id"`
	Kind        string `json:"kind"` // flight, stay or transfer
	Reference   string `json:"reference"`
	Status      string `json:"status"`
	Summary     string `json:"summary"`
	Date        string `json:"date"`               // departure, check-in or transfer day
	EndDate     string `json:"end_date,omitempty"` // check-out day, stays only
	From        string `json:"from,omitempty"`     // flights and transfers
	To          string `json:"to,omitempty"`       // flights and transfers
	City        string `json:"city,omitempty"`     // stays
	AmountCents int    `json:"amount_cents"`
	Currency    string `json:"currency"`
}

// PaymentMethod is one stored card. Only the brand and the last four digits are kept.
type PaymentMethod struct {
	ID          string `json:"id"`
	TravellerID string `json:"traveller_id"`
	Brand       string `json:"brand"`
	Last4       string `json:"last4"`
	Expiry      string `json:"expiry"`
	Default     bool   `json:"default"`
}

// Charge is one captured payment against a booking.
type Charge struct {
	ID              string `json:"id"`
	BookingID       string `json:"booking_id"`
	PaymentMethodID string `json:"payment_method_id"`
	AmountCents     int    `json:"amount_cents"`
	Currency        string `json:"currency"`
	Status          string `json:"status"`
	CreatedAt       string `json:"created_at"`
}

// Refund is one full refund of a charge.
type Refund struct {
	ID          string `json:"id"`
	ChargeID    string `json:"charge_id"`
	BookingID   string `json:"booking_id"`
	AmountCents int    `json:"amount_cents"`
	Currency    string `json:"currency"`
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// Disruption is one reported delay, cancellation or cancellation by a supplier.
type Disruption struct {
	ID           string `json:"id"`
	BookingID    string `json:"booking_id"`
	TravellerID  string `json:"traveller_id"`
	Kind         string `json:"kind"`
	Reason       string `json:"reason"`
	Status       string `json:"status"`
	NewBookingID string `json:"new_booking_id,omitempty"`
	CreatedAt    string `json:"created_at"`
}

// Callback is one requested human callback.
type Callback struct {
	ID          string `json:"id"`
	TravellerID string `json:"traveller_id,omitempty"`
	Name        string `json:"name"`
	Phone       string `json:"phone"`
	Topic       string `json:"topic"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
}

// Loyalty is a traveller's points balance and progress towards the next tier.
type Loyalty struct {
	TravellerID  string `json:"traveller_id"`
	Name         string `json:"name"`
	Tier         string `json:"tier"`
	Points       int    `json:"points"`
	NextTier     string `json:"next_tier,omitempty"`
	PointsToNext int    `json:"points_to_next,omitempty"`
}

// Server is the fixture data and its MCP server. The zero value is not usable; call New.
type Server struct {
	token string
	now   func() time.Time
	delay time.Duration
	srv   *mcp.Server

	mu          sync.Mutex
	travellers  []Traveller
	bookings    []Booking
	methods     []PaymentMethod
	charges     []Charge
	refunds     []Refund
	disruptions []Disruption
	callbacks   []Callback

	nextBooking    int
	nextCharge     int
	nextRefund     int
	nextDisruption int
	nextCallback   int
}

// New builds the fixture server. It fails when no token is set, because /mcp must never be open.
func New(o Options) (*Server, error) {
	if o.Token == "" {
		return nil, ErrNoToken
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Delay < 0 {
		o.Delay = 0
	}
	s := &Server{
		token:          o.Token,
		now:            func() time.Time { return o.Now().UTC() },
		delay:          o.Delay,
		nextBooking:    1,
		nextCharge:     1,
		nextRefund:     1,
		nextDisruption: 1,
		nextCallback:   1,
	}
	if err := s.seed(); err != nil {
		return nil, err
	}
	s.srv = mcp.NewServer(&mcp.Implementation{Name: "atlas-travel-concierge", Version: "1"}, nil)
	s.addTools()
	return s, nil
}

// Handler serves GET /health, the two JSON fixtures under /api and the bearer-token /mcp endpoint.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/weather", s.handleWeather)
	mux.HandleFunc("GET /api/advisory", s.handleAdvisory)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.srv }, nil)
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	return mux
}

// weatherReply is the JSON body of GET /api/weather.
type weatherReply struct {
	City                string `json:"city"`
	Date                string `json:"date"`
	Condition           string `json:"condition"`
	HighC               int    `json:"high_c"`
	LowC                int    `json:"low_c"`
	PrecipitationChance int    `json:"precipitation_chance"`
}

func (s *Server) handleWeather(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	city, ok := canonicalCity(query.Get("city"))
	if !ok {
		writeError(w, http.StatusBadRequest, "city must be London, Paris, New York, Dubai or Singapore")
		return
	}
	day, err := s.dayOrDefault(query.Get("date"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	f := weatherByCity[city]
	writeJSON(w, http.StatusOK, weatherReply{
		City:                city,
		Date:                day.Format(dateLayout),
		Condition:           f.Condition,
		HighC:               f.HighC,
		LowC:                f.LowC,
		PrecipitationChance: f.RainChance,
	})
}

// advisoryReply is the JSON body of GET /api/advisory.
type advisoryReply struct {
	Country string `json:"country"`
	Level   string `json:"level"`
	Summary string `json:"summary"`
	Updated string `json:"updated"`
}

func (s *Server) handleAdvisory(w http.ResponseWriter, r *http.Request) {
	country, ok := canonicalCountry(r.URL.Query().Get("country"))
	if !ok {
		writeError(w, http.StatusBadRequest, "country must be United Kingdom, France, United States, United Arab Emirates or Singapore")
		return
	}
	a := advisoryByCountry[country]
	writeJSON(w, http.StatusOK, advisoryReply{
		Country: country,
		Level:   a.Level,
		Summary: a.Summary,
		Updated: s.today().Format(dateLayout),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{message})
}

// seed adds the three fixed travellers, their payment methods and one future booking each.
// It runs before the server is shared, so it deliberately does not take the lock.
func (s *Server) seed() error {
	profiles := []struct {
		name, tier, email, phone string
		points                   int
	}{
		{"Maria Lopez", "Gold", "maria.lopez@example.com", "555-0142", 8250},
		{"James Okafor", "Silver", "james.okafor@example.com", "555-0188", 3400},
		{"Priya Nair", "Platinum", "priya.nair@example.com", "555-0107", 21700},
	}
	for i, p := range profiles {
		s.travellers = append(s.travellers, Traveller{
			ID:     "t" + strconv.Itoa(i+1),
			Name:   p.name,
			Tier:   p.tier,
			Email:  p.email,
			Phone:  p.phone,
			Points: p.points,
		})
	}

	cards := []struct {
		traveller, brand, last4, expiry string
		def                             bool
	}{
		{"t1", "Visa", "4242", "09/28", true},
		{"t1", "Mastercard", "4444", "03/29", false},
		{"t2", "Visa", "1881", "11/27", true},
		{"t2", "American Express", "0005", "06/28", false},
		{"t3", "Visa", "9003", "01/30", true},
		{"t3", "American Express", "3007", "08/29", false},
	}
	for i, c := range cards {
		s.methods = append(s.methods, PaymentMethod{
			ID:          "pm" + strconv.Itoa(i+1),
			TravellerID: c.traveller,
			Brand:       c.brand,
			Last4:       c.last4,
			Expiry:      c.expiry,
			Default:     c.def,
		})
	}

	today := s.today()
	// b1: Maria flies London to Paris in three days.
	if _, err := s.storeBookingLocked(Booking{
		TravellerID: "t1",
		Kind:        kindFlight,
		Summary:     "London to Paris",
		Date:        today.AddDate(0, 0, 3).Format(dateLayout),
		From:        "London",
		To:          "Paris",
		AmountCents: legPrice("London", "Paris", 0),
	}); err != nil {
		return err
	}
	// b2: James flies Paris to Dubai in five days.
	if _, err := s.storeBookingLocked(Booking{
		TravellerID: "t2",
		Kind:        kindFlight,
		Summary:     "Paris to Dubai",
		Date:        today.AddDate(0, 0, 5).Format(dateLayout),
		From:        "Paris",
		To:          "Dubai",
		AmountCents: legPrice("Paris", "Dubai", 0),
	}); err != nil {
		return err
	}
	// b3: Priya stays in Singapore for two nights in a week.
	checkIn := today.AddDate(0, 0, 7).Format(dateLayout)
	checkOut := today.AddDate(0, 0, 9).Format(dateLayout)
	if _, err := s.storeBookingLocked(Booking{
		TravellerID: "t3",
		Kind:        kindStay,
		Summary:     "Marina Bay House, Singapore",
		Date:        checkIn,
		EndDate:     checkOut,
		City:        "Singapore",
		AmountCents: stayPerNight("Singapore", 0) * 2,
	}); err != nil {
		return err
	}
	return nil
}

// storeBookingLocked appends a booking, assigning its id and supplier reference. The caller holds
// s.mu (seed is the only exception, and it runs before the server is shared).
func (s *Server) storeBookingLocked(b Booking) (Booking, error) {
	if len(s.bookings) >= MaxBookings {
		return Booking{}, errors.New("the booking list is full; cancel an existing booking first")
	}
	b.ID = "b" + strconv.Itoa(s.nextBooking)
	b.Reference = reference(s.nextBooking)
	s.nextBooking++
	if b.Status == "" {
		b.Status = statusConfirmed
	}
	if b.Currency == "" {
		b.Currency = currency
	}
	s.bookings = append(s.bookings, b)
	return b, nil
}

// storeChargeLocked appends a captured charge. The caller holds s.mu.
func (s *Server) storeChargeLocked(c Charge) (Charge, error) {
	if len(s.charges) >= maxCharges {
		return Charge{}, errors.New("the charge list is full")
	}
	c.ID = "ch" + strconv.Itoa(s.nextCharge)
	s.nextCharge++
	if c.Status == "" {
		c.Status = "captured"
	}
	if c.Currency == "" {
		c.Currency = currency
	}
	c.CreatedAt = s.now().Format(time.RFC3339)
	s.charges = append(s.charges, c)
	return c, nil
}

// storeRefundLocked appends a refund. The caller holds s.mu.
func (s *Server) storeRefundLocked(r Refund) (Refund, error) {
	if len(s.refunds) >= maxRefunds {
		return Refund{}, errors.New("the refund list is full")
	}
	r.ID = "rf" + strconv.Itoa(s.nextRefund)
	s.nextRefund++
	if r.Status == "" {
		r.Status = "refunded"
	}
	if r.Currency == "" {
		r.Currency = currency
	}
	r.CreatedAt = s.now().Format(time.RFC3339)
	s.refunds = append(s.refunds, r)
	return r, nil
}

// storeDisruptionLocked appends a disruption. The caller holds s.mu.
func (s *Server) storeDisruptionLocked(d Disruption) (Disruption, error) {
	if len(s.disruptions) >= maxDisruptions {
		return Disruption{}, errors.New("the disruption list is full")
	}
	d.ID = "d" + strconv.Itoa(s.nextDisruption)
	s.nextDisruption++
	if d.Status == "" {
		d.Status = disruptionReported
	}
	d.CreatedAt = s.now().Format(time.RFC3339)
	s.disruptions = append(s.disruptions, d)
	return d, nil
}

// storeCallbackLocked appends a callback. The caller holds s.mu.
func (s *Server) storeCallbackLocked(c Callback) (Callback, error) {
	if len(s.callbacks) >= MaxCallbacks {
		return Callback{}, errors.New("the callback queue is full")
	}
	c.ID = "cb" + strconv.Itoa(s.nextCallback)
	s.nextCallback++
	if c.Status == "" {
		c.Status = "requested"
	}
	c.CreatedAt = s.now().Format(time.RFC3339)
	s.callbacks = append(s.callbacks, c)
	return c, nil
}

// travellerByID finds a traveller by exact id. The caller holds s.mu.
func (s *Server) travellerByID(id string) (Traveller, bool) {
	for _, t := range s.travellers {
		if t.ID == id {
			return t, true
		}
	}
	return Traveller{}, false
}

// travellerByName finds a traveller by full name, ignoring case and spacing. The caller holds s.mu.
func (s *Server) travellerByName(name string) (Traveller, bool) {
	want := strings.ToLower(strings.Join(strings.Fields(name), " "))
	for _, t := range s.travellers {
		if strings.ToLower(strings.Join(strings.Fields(t.Name), " ")) == want {
			return t, true
		}
	}
	return Traveller{}, false
}

// setTravellerLocked replaces a traveller's mutable fields. The caller holds s.mu.
func (s *Server) setTravellerLocked(t Traveller) {
	for i := range s.travellers {
		if s.travellers[i].ID == t.ID {
			s.travellers[i] = t
			return
		}
	}
}

// bookingIndex finds a booking's position by id. The caller holds s.mu.
func (s *Server) bookingIndex(id string) (int, bool) {
	for i, b := range s.bookings {
		if b.ID == id {
			return i, true
		}
	}
	return 0, false
}

// paymentByID finds a payment method by id. The caller holds s.mu.
func (s *Server) paymentByID(id string) (PaymentMethod, bool) {
	for _, m := range s.methods {
		if m.ID == id {
			return m, true
		}
	}
	return PaymentMethod{}, false
}

// chargeIndex finds a charge's position by id. The caller holds s.mu.
func (s *Server) chargeIndex(id string) (int, bool) {
	for i, c := range s.charges {
		if c.ID == id {
			return i, true
		}
	}
	return 0, false
}

// chargeForBooking reports the charge of a booking, if it was already charged. The caller holds s.mu.
func (s *Server) chargeForBooking(bookingID string) (Charge, bool) {
	for _, c := range s.charges {
		if c.BookingID == bookingID {
			return c, true
		}
	}
	return Charge{}, false
}

// refundForCharge reports the refund of a charge, if it was already refunded. The caller holds s.mu.
func (s *Server) refundForCharge(chargeID string) (Refund, bool) {
	for _, r := range s.refunds {
		if r.ChargeID == chargeID {
			return r, true
		}
	}
	return Refund{}, false
}

// disruptionIndex finds a disruption's position by id. The caller holds s.mu.
func (s *Server) disruptionIndex(id string) (int, bool) {
	for i, d := range s.disruptions {
		if d.ID == id {
			return i, true
		}
	}
	return 0, false
}

// tierFor is the loyalty tier a points balance earns.
func tierFor(points int) string {
	switch {
	case points >= 15000:
		return "Platinum"
	case points >= 5000:
		return "Gold"
	default:
		return "Silver"
	}
}

// loyaltyFor describes a traveller's points balance and the distance to the next tier.
func loyaltyFor(t Traveller) Loyalty {
	l := Loyalty{TravellerID: t.ID, Name: t.Name, Tier: t.Tier, Points: t.Points}
	switch t.Tier {
	case "Silver":
		l.NextTier, l.PointsToNext = "Gold", 5000-t.Points
	case "Gold":
		l.NextTier, l.PointsToNext = "Platinum", 15000-t.Points
	}
	return l
}

// wait sleeps for d, returning early when ctx ends. A zero delay returns at once.
func wait(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
