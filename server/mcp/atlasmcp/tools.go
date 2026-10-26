package atlasmcp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool input and output shapes. Required fields have no omitempty, so the SDK's derived JSON
// schema marks them required; optional fields are omitempty. Every name is snake_case because
// the model and the HTTP tool definitions read them.

type getTravellerIn struct {
	TravellerID string `json:"traveller_id,omitempty"`
	Name        string `json:"name,omitempty"`
}
type getTravellerOut struct {
	Found     bool       `json:"found"`
	Traveller *Traveller `json:"traveller,omitempty"`
}

type listBookingsIn struct {
	TravellerID string `json:"traveller_id"`
}
type listBookingsOut struct {
	Bookings []Booking `json:"bookings"`
}

type searchFlightsIn struct {
	From string `json:"from"`
	To   string `json:"to"`
	Date string `json:"date,omitempty"`
}
type searchFlightsOut struct {
	From    string   `json:"from"`
	To      string   `json:"to"`
	Date    string   `json:"date"`
	Flights []Flight `json:"flights"`
}

type bookFlightIn struct {
	TravellerID string `json:"traveller_id"`
	FlightID    string `json:"flight_id"`
}
type bookingOut struct {
	Booking Booking `json:"booking"`
}

type changeFlightIn struct {
	BookingID string `json:"booking_id"`
	FlightID  string `json:"flight_id"`
}

type cancelBookingIn struct {
	BookingID string `json:"booking_id"`
}
type cancelBookingOut struct {
	BookingID string `json:"booking_id"`
	Status    string `json:"status"`
}

type searchStaysIn struct {
	City     string `json:"city"`
	CheckIn  string `json:"check_in,omitempty"`
	CheckOut string `json:"check_out,omitempty"`
}
type searchStaysOut struct {
	City     string `json:"city"`
	CheckIn  string `json:"check_in"`
	CheckOut string `json:"check_out"`
	Nights   int    `json:"nights"`
	Stays    []Stay `json:"stays"`
}

type bookStayIn struct {
	TravellerID string `json:"traveller_id"`
	StayID      string `json:"stay_id"`
}

type searchTransfersIn struct {
	From string `json:"from"`
	To   string `json:"to"`
	Date string `json:"date,omitempty"`
}
type searchTransfersOut struct {
	From      string     `json:"from"`
	To        string     `json:"to"`
	Date      string     `json:"date"`
	Transfers []Transfer `json:"transfers"`
}

type bookTransferIn struct {
	TravellerID string `json:"traveller_id"`
	TransferID  string `json:"transfer_id"`
}

type listPaymentMethodsIn struct {
	TravellerID string `json:"traveller_id"`
}
type listPaymentMethodsOut struct {
	PaymentMethods []PaymentMethod `json:"payment_methods"`
}

type chargeBookingIn struct {
	BookingID       string `json:"booking_id"`
	PaymentMethodID string `json:"payment_method_id"`
}
type chargeBookingOut struct {
	Charge Charge `json:"charge"`
}

type issueRefundIn struct {
	ChargeID string `json:"charge_id"`
	Reason   string `json:"reason,omitempty"`
}
type issueRefundOut struct {
	Refund Refund `json:"refund"`
}

type getLoyaltyIn struct {
	TravellerID string `json:"traveller_id"`
}
type getLoyaltyOut struct {
	Loyalty Loyalty `json:"loyalty"`
}

type applyPointsIn struct {
	TravellerID string `json:"traveller_id"`
	Points      int    `json:"points"`
	Reason      string `json:"reason,omitempty"`
}
type applyPointsOut struct {
	AppliedPoints int     `json:"applied_points"`
	Loyalty       Loyalty `json:"loyalty"`
}

type reportDisruptionIn struct {
	BookingID string `json:"booking_id"`
	Reason    string `json:"reason,omitempty"`
}
type reportDisruptionOut struct {
	Disruption Disruption `json:"disruption"`
}

type rebookDisruptionIn struct {
	DisruptionID string `json:"disruption_id"`
}
type rebookDisruptionOut struct {
	Disruption Disruption `json:"disruption"`
	Booking    Booking    `json:"booking"`
}

type createCallbackIn struct {
	TravellerID string `json:"traveller_id,omitempty"`
	Name        string `json:"name,omitempty"`
	Phone       string `json:"phone,omitempty"`
	Topic       string `json:"topic"`
}
type createCallbackOut struct {
	Callback Callback `json:"callback"`
}

// addTools registers the eighteen MCP tools in ToolNames order.
func (s *Server) addTools() {
	mcp.AddTool(s.srv, &mcp.Tool{Name: "get_traveller", Description: "Look up a traveller by traveller_id or full name. found is false when nobody matches, so ask the caller to confirm their name."},
		func(_ context.Context, _ *mcp.CallToolRequest, in getTravellerIn) (*mcp.CallToolResult, getTravellerOut, error) {
			name := strings.TrimSpace(in.Name)
			if utf8.RuneCountInString(name) > maxField {
				return nil, getTravellerOut{}, fmt.Errorf("name must be at most %d characters", maxField)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			var t Traveller
			var ok bool
			switch {
			case ref(in.TravellerID) != "":
				t, ok = s.travellerByID(ref(in.TravellerID))
			case name != "":
				t, ok = s.travellerByName(name)
			default:
				return nil, getTravellerOut{}, errors.New("give traveller_id or name")
			}
			if !ok {
				return nil, getTravellerOut{}, nil
			}
			return nil, getTravellerOut{Found: true, Traveller: &t}, nil
		})

	mcp.AddTool(s.srv, &mcp.Tool{Name: "list_bookings", Description: "List a traveller's upcoming confirmed bookings."},
		func(_ context.Context, _ *mcp.CallToolRequest, in listBookingsIn) (*mcp.CallToolResult, listBookingsOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			t, ok := s.travellerByID(ref(in.TravellerID))
			if !ok {
				return nil, listBookingsOut{}, fmt.Errorf("unknown traveller_id %q", ref(in.TravellerID))
			}
			out := make([]Booking, 0, len(s.bookings))
			for _, b := range s.bookings {
				if b.TravellerID == t.ID && b.Status == statusConfirmed {
					out = append(out, b)
				}
			}
			slices.SortFunc(out, func(a, b Booking) int {
				if a.Date != b.Date {
					return strings.Compare(a.Date, b.Date)
				}
				return strings.Compare(a.ID, b.ID)
			})
			return nil, listBookingsOut{Bookings: out}, nil
		})

	mcp.AddTool(s.srv, &mcp.Tool{Name: "search_flights", Description: "Find flights between two of the demo cities (London, Paris, New York, Dubai, Singapore). date is YYYY-MM-DD and defaults to tomorrow."},
		func(_ context.Context, _ *mcp.CallToolRequest, in searchFlightsIn) (*mcp.CallToolResult, searchFlightsOut, error) {
			from, to, err := s.endpoints(in.From, in.To)
			if err != nil {
				return nil, searchFlightsOut{}, err
			}
			day, err := s.dayOrDefault(ref(in.Date))
			if err != nil {
				return nil, searchFlightsOut{}, err
			}
			date := day.Format(dateLayout)
			return nil, searchFlightsOut{From: from, To: to, Date: date, Flights: flightsBetween(from, to, date)}, nil
		})

	mcp.AddTool(s.srv, &mcp.Tool{Name: "book_flight", Description: "Book a flight_id from search_flights for a traveller. Confirm the price with the caller before calling."},
		func(_ context.Context, _ *mcp.CallToolRequest, in bookFlightIn) (*mcp.CallToolResult, bookingOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			t, ok := s.travellerByID(ref(in.TravellerID))
			if !ok {
				return nil, bookingOut{}, fmt.Errorf("unknown traveller_id %q", ref(in.TravellerID))
			}
			flight, err := s.flightByID(ref(in.FlightID))
			if err != nil {
				return nil, bookingOut{}, err
			}
			b, err := s.storeBookingLocked(Booking{
				TravellerID: t.ID,
				Kind:        kindFlight,
				Summary:     flight.From + " to " + flight.To,
				Date:        flight.Date,
				From:        flight.From,
				To:          flight.To,
				AmountCents: flight.PriceCents,
			})
			return nil, bookingOut{Booking: b}, err
		})

	mcp.AddTool(s.srv, &mcp.Tool{Name: "change_flight", Description: "Move an existing flight booking to a flight_id from search_flights."},
		func(_ context.Context, _ *mcp.CallToolRequest, in changeFlightIn) (*mcp.CallToolResult, bookingOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			idx, ok := s.bookingIndex(ref(in.BookingID))
			if !ok {
				return nil, bookingOut{}, fmt.Errorf("unknown booking_id %q", ref(in.BookingID))
			}
			b := s.bookings[idx]
			if b.Kind != kindFlight {
				return nil, bookingOut{}, fmt.Errorf("booking %s is a %s booking; change_flight only moves flights", b.ID, b.Kind)
			}
			if b.Status != statusConfirmed {
				return nil, bookingOut{}, fmt.Errorf("booking %s is %s and cannot be changed", b.ID, b.Status)
			}
			flight, err := s.flightByID(ref(in.FlightID))
			if err != nil {
				return nil, bookingOut{}, err
			}
			b.Summary, b.Date, b.From, b.To = flight.From+" to "+flight.To, flight.Date, flight.From, flight.To
			b.AmountCents = flight.PriceCents
			s.bookings[idx] = b
			return nil, bookingOut{Booking: b}, nil
		})

	mcp.AddTool(s.srv, &mcp.Tool{Name: "cancel_booking", Description: "Cancel a confirmed booking. Confirm with the caller before calling."},
		func(_ context.Context, _ *mcp.CallToolRequest, in cancelBookingIn) (*mcp.CallToolResult, cancelBookingOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			idx, ok := s.bookingIndex(ref(in.BookingID))
			if !ok {
				return nil, cancelBookingOut{}, fmt.Errorf("unknown booking_id %q", ref(in.BookingID))
			}
			b := s.bookings[idx]
			if b.Status != statusConfirmed {
				return nil, cancelBookingOut{}, fmt.Errorf("booking %s is already %s", b.ID, b.Status)
			}
			s.bookings[idx].Status = statusCancelled
			return nil, cancelBookingOut{BookingID: b.ID, Status: statusCancelled}, nil
		})

	mcp.AddTool(s.srv, &mcp.Tool{Name: "search_stays", Description: "Find hotels in one demo city. check_in is YYYY-MM-DD and defaults to tomorrow; check_out defaults to two nights later."},
		func(_ context.Context, _ *mcp.CallToolRequest, in searchStaysIn) (*mcp.CallToolResult, searchStaysOut, error) {
			city, ok := canonicalCity(in.City)
			if !ok {
				return nil, searchStaysOut{}, fmt.Errorf("unknown city %q; use London, Paris, New York, Dubai or Singapore", ref(in.City))
			}
			checkIn, checkOut, nights, err := s.stayDates(ref(in.CheckIn), ref(in.CheckOut))
			if err != nil {
				return nil, searchStaysOut{}, err
			}
			return nil, searchStaysOut{City: city, CheckIn: checkIn, CheckOut: checkOut, Nights: nights, Stays: staysIn(city, checkIn, checkOut, nights)}, nil
		})

	mcp.AddTool(s.srv, &mcp.Tool{Name: "book_stay", Description: "Book a stay_id from search_stays for a traveller. Confirm the price with the caller before calling."},
		func(_ context.Context, _ *mcp.CallToolRequest, in bookStayIn) (*mcp.CallToolResult, bookingOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			t, ok := s.travellerByID(ref(in.TravellerID))
			if !ok {
				return nil, bookingOut{}, fmt.Errorf("unknown traveller_id %q", ref(in.TravellerID))
			}
			stay, err := s.stayByID(ref(in.StayID))
			if err != nil {
				return nil, bookingOut{}, err
			}
			b, err := s.storeBookingLocked(Booking{
				TravellerID: t.ID,
				Kind:        kindStay,
				Summary:     stay.Name + ", " + stay.City,
				Date:        stay.CheckIn,
				EndDate:     stay.CheckOut,
				City:        stay.City,
				AmountCents: stay.TotalCents,
			})
			return nil, bookingOut{Booking: b}, err
		})

	mcp.AddTool(s.srv, &mcp.Tool{Name: "search_transfers", Description: "Find ground transfers between two of the demo cities. date is YYYY-MM-DD and defaults to tomorrow."},
		func(_ context.Context, _ *mcp.CallToolRequest, in searchTransfersIn) (*mcp.CallToolResult, searchTransfersOut, error) {
			from, to, err := s.endpoints(in.From, in.To)
			if err != nil {
				return nil, searchTransfersOut{}, err
			}
			day, err := s.dayOrDefault(ref(in.Date))
			if err != nil {
				return nil, searchTransfersOut{}, err
			}
			date := day.Format(dateLayout)
			return nil, searchTransfersOut{From: from, To: to, Date: date, Transfers: transfersBetween(from, to, date)}, nil
		})

	mcp.AddTool(s.srv, &mcp.Tool{Name: "book_transfer", Description: "Book a transfer_id from search_transfers for a traveller. Confirm the price with the caller before calling."},
		func(_ context.Context, _ *mcp.CallToolRequest, in bookTransferIn) (*mcp.CallToolResult, bookingOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			t, ok := s.travellerByID(ref(in.TravellerID))
			if !ok {
				return nil, bookingOut{}, fmt.Errorf("unknown traveller_id %q", ref(in.TravellerID))
			}
			transfer, err := s.transferByID(ref(in.TransferID))
			if err != nil {
				return nil, bookingOut{}, err
			}
			b, err := s.storeBookingLocked(Booking{
				TravellerID: t.ID,
				Kind:        kindTransfer,
				Summary:     transfer.From + " to " + transfer.To,
				Date:        transfer.Date,
				From:        transfer.From,
				To:          transfer.To,
				AmountCents: transfer.PriceCents,
			})
			return nil, bookingOut{Booking: b}, err
		})

	mcp.AddTool(s.srv, &mcp.Tool{Name: "list_payment_methods", Description: "List a traveller's stored payment methods. Only the brand and the last four digits are returned."},
		func(_ context.Context, _ *mcp.CallToolRequest, in listPaymentMethodsIn) (*mcp.CallToolResult, listPaymentMethodsOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			t, ok := s.travellerByID(ref(in.TravellerID))
			if !ok {
				return nil, listPaymentMethodsOut{}, fmt.Errorf("unknown traveller_id %q", ref(in.TravellerID))
			}
			out := make([]PaymentMethod, 0, len(s.methods))
			for _, m := range s.methods {
				if m.TravellerID == t.ID {
					out = append(out, m)
				}
			}
			return nil, listPaymentMethodsOut{PaymentMethods: out}, nil
		})

	mcp.AddTool(s.srv, &mcp.Tool{Name: "charge_booking", Description: "Charge a confirmed booking to one of the traveller's payment methods and return the charge id."},
		func(_ context.Context, _ *mcp.CallToolRequest, in chargeBookingIn) (*mcp.CallToolResult, chargeBookingOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			idx, ok := s.bookingIndex(ref(in.BookingID))
			if !ok {
				return nil, chargeBookingOut{}, fmt.Errorf("unknown booking_id %q", ref(in.BookingID))
			}
			b := s.bookings[idx]
			if b.Status != statusConfirmed {
				return nil, chargeBookingOut{}, fmt.Errorf("booking %s is %s; only a confirmed booking can be charged", b.ID, b.Status)
			}
			method, ok := s.paymentByID(ref(in.PaymentMethodID))
			if !ok {
				return nil, chargeBookingOut{}, fmt.Errorf("unknown payment_method_id %q; call list_payment_methods first", in.PaymentMethodID)
			}
			if method.TravellerID != b.TravellerID {
				return nil, chargeBookingOut{}, fmt.Errorf("payment method %s belongs to another traveller", method.ID)
			}
			if existing, charged := s.chargeForBooking(b.ID); charged {
				return nil, chargeBookingOut{}, fmt.Errorf("booking %s is already charged (%s)", b.ID, existing.ID)
			}
			c, err := s.storeChargeLocked(Charge{BookingID: b.ID, PaymentMethodID: method.ID, AmountCents: b.AmountCents})
			return nil, chargeBookingOut{Charge: c}, err
		})

	mcp.AddTool(s.srv, &mcp.Tool{Name: "issue_refund", Description: "Refund a captured charge in full and return the refund id."},
		func(_ context.Context, _ *mcp.CallToolRequest, in issueRefundIn) (*mcp.CallToolResult, issueRefundOut, error) {
			reason, err := optionalText("reason", in.Reason, "")
			if err != nil {
				return nil, issueRefundOut{}, err
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			idx, ok := s.chargeIndex(ref(in.ChargeID))
			if !ok {
				return nil, issueRefundOut{}, fmt.Errorf("unknown charge_id %q", ref(in.ChargeID))
			}
			c := s.charges[idx]
			if existing, done := s.refundForCharge(c.ID); done {
				return nil, issueRefundOut{}, fmt.Errorf("charge %s was already refunded (%s)", c.ID, existing.ID)
			}
			r, err := s.storeRefundLocked(Refund{ChargeID: c.ID, BookingID: c.BookingID, AmountCents: c.AmountCents, Reason: reason})
			if err != nil {
				return nil, issueRefundOut{}, err
			}
			s.charges[idx].Status = "refunded"
			return nil, issueRefundOut{Refund: r}, nil
		})

	mcp.AddTool(s.srv, &mcp.Tool{Name: "get_loyalty", Description: "Show a traveller's loyalty tier, points balance and points to the next tier."},
		func(_ context.Context, _ *mcp.CallToolRequest, in getLoyaltyIn) (*mcp.CallToolResult, getLoyaltyOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			t, ok := s.travellerByID(ref(in.TravellerID))
			if !ok {
				return nil, getLoyaltyOut{}, fmt.Errorf("unknown traveller_id %q", ref(in.TravellerID))
			}
			return nil, getLoyaltyOut{Loyalty: loyaltyFor(t)}, nil
		})

	mcp.AddTool(s.srv, &mcp.Tool{Name: "apply_points", Description: "Redeem loyalty points from a traveller's balance. points must be positive and no more than the balance."},
		func(_ context.Context, _ *mcp.CallToolRequest, in applyPointsIn) (*mcp.CallToolResult, applyPointsOut, error) {
			if _, err := optionalText("reason", in.Reason, ""); err != nil {
				return nil, applyPointsOut{}, err
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			t, ok := s.travellerByID(ref(in.TravellerID))
			if !ok {
				return nil, applyPointsOut{}, fmt.Errorf("unknown traveller_id %q", ref(in.TravellerID))
			}
			if in.Points <= 0 {
				return nil, applyPointsOut{}, fmt.Errorf("points must be positive, got %d", in.Points)
			}
			if in.Points > t.Points {
				return nil, applyPointsOut{}, fmt.Errorf("traveller %s has %d points; cannot apply %d", t.ID, t.Points, in.Points)
			}
			t.Points -= in.Points
			t.Tier = tierFor(t.Points)
			s.setTravellerLocked(t)
			return nil, applyPointsOut{AppliedPoints: in.Points, Loyalty: loyaltyFor(t)}, nil
		})

	mcp.AddTool(s.srv, &mcp.Tool{Name: "report_disruption", Description: "Report a delay, cancellation or overbooking on a confirmed booking and return the disruption id."},
		func(_ context.Context, _ *mcp.CallToolRequest, in reportDisruptionIn) (*mcp.CallToolResult, reportDisruptionOut, error) {
			reason, err := optionalText("reason", in.Reason, "Operational disruption reported by the traveller")
			if err != nil {
				return nil, reportDisruptionOut{}, err
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			idx, ok := s.bookingIndex(ref(in.BookingID))
			if !ok {
				return nil, reportDisruptionOut{}, fmt.Errorf("unknown booking_id %q", ref(in.BookingID))
			}
			b := s.bookings[idx]
			if b.Status != statusConfirmed {
				return nil, reportDisruptionOut{}, fmt.Errorf("booking %s is %s; only a confirmed booking can be disrupted", b.ID, b.Status)
			}
			d, err := s.storeDisruptionLocked(Disruption{BookingID: b.ID, TravellerID: b.TravellerID, Kind: b.Kind, Reason: reason})
			return nil, reportDisruptionOut{Disruption: d}, err
		})

	mcp.AddTool(s.srv, &mcp.Tool{Name: "rebook_disruption", Description: "Rebook a reported disruption for the next day. The work runs in the background and can take a moment; the answer names the replacement booking."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in rebookDisruptionIn) (*mcp.CallToolResult, rebookDisruptionOut, error) {
			id := ref(in.DisruptionID)
			s.mu.Lock()
			idx, ok := s.disruptionIndex(id)
			if !ok {
				s.mu.Unlock()
				return nil, rebookDisruptionOut{}, fmt.Errorf("unknown disruption_id %q", ref(in.DisruptionID))
			}
			if status := s.disruptions[idx].Status; status != disruptionReported {
				s.mu.Unlock()
				return nil, rebookDisruptionOut{}, fmt.Errorf("disruption %s is already %s", s.disruptions[idx].ID, status)
			}
			if _, ok := s.bookingOf(s.disruptions[idx]); !ok {
				s.mu.Unlock()
				return nil, rebookDisruptionOut{}, fmt.Errorf("the booking of disruption %s is no longer available", id)
			}
			s.mu.Unlock()
			if err := wait(ctx, s.delay); err != nil {
				return nil, rebookDisruptionOut{}, fmt.Errorf("rebooking was cancelled: %w", err)
			}

			s.mu.Lock()
			defer s.mu.Unlock()
			// Read again: a concurrent call may have rebooked the same disruption while this one waited.
			idx, ok = s.disruptionIndex(id)
			if !ok {
				return nil, rebookDisruptionOut{}, fmt.Errorf("unknown disruption_id %q", ref(in.DisruptionID))
			}
			if status := s.disruptions[idx].Status; status != disruptionReported {
				return nil, rebookDisruptionOut{}, fmt.Errorf("disruption %s is already %s", s.disruptions[idx].ID, status)
			}
			bookingIdx, ok := s.bookingIndex(s.disruptions[idx].BookingID)
			if !ok {
				return nil, rebookDisruptionOut{}, fmt.Errorf("the booking of disruption %s is no longer available", id)
			}
			if s.bookings[bookingIdx].Status != statusConfirmed {
				return nil, rebookDisruptionOut{}, fmt.Errorf("booking %s is %s and cannot be rebooked", s.bookings[bookingIdx].ID, s.bookings[bookingIdx].Status)
			}
			replacement, err := s.replacementLocked(s.bookings[bookingIdx])
			if err != nil {
				return nil, rebookDisruptionOut{}, err
			}
			s.bookings[bookingIdx].Status = statusRebooked
			s.disruptions[idx].Status = disruptionRebooked
			s.disruptions[idx].NewBookingID = replacement.ID
			return nil, rebookDisruptionOut{Disruption: s.disruptions[idx], Booking: replacement}, nil
		})

	mcp.AddTool(s.srv, &mcp.Tool{Name: "create_callback", Description: "Request a human callback and return the callback id. Give either traveller_id, or the caller's name and phone together; topic is required."},
		func(_ context.Context, _ *mcp.CallToolRequest, in createCallbackIn) (*mcp.CallToolResult, createCallbackOut, error) {
			topic, err := requiredText("topic", in.Topic)
			if err != nil {
				return nil, createCallbackOut{}, err
			}
			caller, err := s.callbackCaller(in)
			if err != nil {
				return nil, createCallbackOut{}, err
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			cb, err := s.storeCallbackLocked(Callback{TravellerID: caller.ID, Name: caller.Name, Phone: caller.Phone, Topic: topic})
			return nil, createCallbackOut{Callback: cb}, err
		})
}

// callbackCaller resolves who a callback is for: a seeded traveller by id, or the name and phone
// the caller gave. A caller who is not in the fixtures still gets a callback stored under that
// name and phone, without a traveller id.
func (s *Server) callbackCaller(in createCallbackIn) (Traveller, error) {
	if ref(in.TravellerID) != "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		t, ok := s.travellerByID(ref(in.TravellerID))
		if !ok {
			return Traveller{}, fmt.Errorf("unknown traveller_id %q; call get_traveller or give the caller's name and phone", ref(in.TravellerID))
		}
		return t, nil
	}
	name, err := requiredText("name", in.Name)
	if err != nil {
		return Traveller{}, fmt.Errorf("give traveller_id, or both name and phone: %w", err)
	}
	phone, err := requiredText("phone", in.Phone)
	if err != nil {
		return Traveller{}, fmt.Errorf("give traveller_id, or both name and phone: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.travellerByName(name); ok && digits(t.Phone) == digits(phone) {
		return t, nil
	}
	return Traveller{Name: name, Phone: phone}, nil
}

// requiredText trims a required text field and enforces the field cap.
func requiredText(field, raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	if utf8.RuneCountInString(v) > maxField {
		return "", fmt.Errorf("%s must be at most %d characters", field, maxField)
	}
	return v, nil
}

// endpoints validates the two city arguments of a search tool.
func (s *Server) endpoints(fromRaw, toRaw string) (from, to string, err error) {
	from, ok := canonicalCity(fromRaw)
	if !ok {
		return "", "", fmt.Errorf("unknown city %q; use London, Paris, New York, Dubai or Singapore", ref(fromRaw))
	}
	to, ok = canonicalCity(toRaw)
	if !ok {
		return "", "", fmt.Errorf("unknown city %q; use London, Paris, New York, Dubai or Singapore", ref(toRaw))
	}
	if from == to {
		return "", "", fmt.Errorf("from and to must be different cities; both were %s", from)
	}
	return from, to, nil
}

// bookingOf returns a copy of a disruption's booking. The caller holds s.mu.
func (s *Server) bookingOf(d Disruption) (Booking, bool) {
	idx, ok := s.bookingIndex(d.BookingID)
	if !ok {
		return Booking{}, false
	}
	return s.bookings[idx], true
}

// replacementLocked builds the replacement booking of a disrupted one: the next day for a flight
// or transfer, or a stay of the same length from tomorrow, inside the booking window. The caller
// holds s.mu and stores the result.
func (s *Server) replacementLocked(target Booking) (Booking, error) {
	day := s.now().AddDate(0, 0, 1).Format(dateLayout)
	replacement := Booking{TravellerID: target.TravellerID, Kind: target.Kind}
	switch target.Kind {
	case kindFlight:
		if target.From == "" || target.To == "" || target.From == target.To {
			return Booking{}, fmt.Errorf("booking %s has no flight details to rebook", target.ID)
		}
		replacement.Summary = target.From + " to " + target.To
		replacement.Date, replacement.From, replacement.To = day, target.From, target.To
		replacement.AmountCents = flightsBetween(target.From, target.To, day)[0].PriceCents
	case kindTransfer:
		if target.From == "" || target.To == "" || target.From == target.To {
			return Booking{}, fmt.Errorf("booking %s has no transfer details to rebook", target.ID)
		}
		replacement.Summary = target.From + " to " + target.To
		replacement.Date, replacement.From, replacement.To = day, target.From, target.To
		replacement.AmountCents = transfersBetween(target.From, target.To, day)[0].PriceCents
	case kindStay:
		if target.City == "" {
			return Booking{}, fmt.Errorf("booking %s has no stay details to rebook", target.ID)
		}
		originalNights := 2
		if in, err := time.Parse(dateLayout, target.Date); err == nil {
			if out, err := time.Parse(dateLayout, target.EndDate); err == nil {
				originalNights = int(out.Sub(in).Hours() / 24)
			}
		}
		if originalNights < 1 {
			originalNights = 1
		}
		nights := originalNights
		if maxNights := horizonDays - 1; nights > maxNights {
			nights = maxNights
		}
		replacement.Summary, replacement.City = target.Summary, target.City
		replacement.Date = day
		replacement.EndDate = s.now().AddDate(0, 0, 1+nights).Format(dateLayout)
		if target.AmountCents > 0 {
			replacement.AmountCents = target.AmountCents / originalNights * nights
		}
	default:
		return Booking{}, fmt.Errorf("booking %s has an unknown kind %q", target.ID, target.Kind)
	}
	return s.storeBookingLocked(replacement)
}

// ref trims a reference input and clips it to the field cap, so a malformed value cannot bloat
// an error answer. Lookups compare it exactly, so a clipped value simply matches nothing.
func ref(raw string) string {
	v := strings.TrimSpace(raw)
	if utf8.RuneCountInString(v) <= maxField {
		return v
	}
	return string([]rune(v)[:maxField])
}

// optionalText trims an optional text field, applies its default and enforces the field cap.
func optionalText(field, raw, fallback string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		v = fallback
	}
	if utf8.RuneCountInString(v) > maxField {
		return "", fmt.Errorf("%s must be at most %d characters", field, maxField)
	}
	return v, nil
}
