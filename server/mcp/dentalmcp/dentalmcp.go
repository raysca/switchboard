// Package dentalmcp is the scheduling backend of the Smile Dental demo: a small MCP
// server over streamable HTTP with fixed fake patients and in-memory appointments.
// It holds no real data and keeps nothing across restarts. Slots are every weekday
// at four fixed times for the next two weeks, so the demo always has something to offer.
package dentalmcp

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	MaxPatients     = 200
	MaxAppointments = 400
	horizonDays     = 14
	slotsPerReply   = 6
	maxField        = 80
	slotLayout      = "2006-01-02T15:04"
)

var (
	slotTimes = []string{"09:00", "11:00", "14:00", "16:00"}
	kinds     = map[string]bool{"checkup": true, "cleaning": true, "filling": true, "consultation": true}
	// ToolNames are the remote names of the server's tools.
	ToolNames = []string{"find_patient", "register_patient", "list_appointments", "find_slots", "book_appointment", "reschedule_appointment", "cancel_appointment"}

	ErrNoToken = errors.New("dentalmcp: a bearer token is required")
)

type Options struct {
	Token string           // bearer token /mcp expects (required)
	Now   func() time.Time // the clock (UTC); default time.Now
}

type Patient struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Phone string `json:"phone"`
}

type Appointment struct {
	ID        string `json:"id"`
	PatientID string `json:"patient_id"`
	Kind      string `json:"kind"`
	SlotID    string `json:"slot_id"`
	When      string `json:"when"`
}

type Slot struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type Server struct {
	token string
	now   func() time.Time
	srv   *mcp.Server

	mu       sync.Mutex
	patients []Patient
	appts    []Appointment
	nextP    int
	nextA    int
}

func New(o Options) (*Server, error) {
	if o.Token == "" {
		return nil, ErrNoToken
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	s := &Server{token: o.Token, now: func() time.Time { return o.Now().UTC() }, nextP: 1, nextA: 1}
	s.seed()
	s.srv = mcp.NewServer(&mcp.Implementation{Name: "smile-dental-scheduling", Version: "1"}, nil)
	s.addTools()
	return s, nil
}

// Handler serves GET /health (open) and /mcp (bearer token).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
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

// seed adds three fixed patients, each with one appointment on a free early slot.
func (s *Server) seed() {
	today := s.now()
	for i, p := range []struct{ name, phone, kind string }{
		{"Maria Lopez", "555-0142", "cleaning"},
		{"James Okafor", "555-0188", "checkup"},
		{"Priya Nair", "555-0107", "consultation"},
	} {
		pt := s.addPatient(p.name, p.phone)
		slots := s.freeSlots(today.AddDate(0, 0, 2+i), 1)
		if len(slots) > 0 {
			_, _ = s.addAppointment(pt.ID, slots[0].ID, p.kind)
		}
	}
}

func (s *Server) addPatient(name, phone string) Patient {
	p := Patient{ID: "p" + strconv.Itoa(s.nextP), Name: name, Phone: phone}
	s.nextP++
	s.patients = append(s.patients, p)
	return p
}

func norm(name string) string { return strings.ToLower(strings.Join(strings.Fields(name), " ")) }

func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// slotTime parses a slot id and checks it is a real, future, weekday slot inside the horizon.
func (s *Server) slotTime(id string) (time.Time, error) {
	t, err := time.Parse(slotLayout, id)
	hhmm := ""
	if err == nil {
		hhmm = t.Format("15:04")
	}
	ok := false
	for _, st := range slotTimes {
		ok = ok || st == hhmm
	}
	now := s.now()
	if err != nil || t.Format(slotLayout) != id || !ok || t.Weekday() == time.Saturday || t.Weekday() == time.Sunday ||
		!t.After(now) || t.After(now.AddDate(0, 0, horizonDays)) {
		return time.Time{}, fmt.Errorf("%q is not an available slot; use an id from find_slots", id)
	}
	return t, nil
}

func (s *Server) taken(slotID, exceptAppt string) bool {
	for _, a := range s.appts {
		if a.SlotID == slotID && a.ID != exceptAppt {
			return true
		}
	}
	return false
}

// freeSlots lists up to limit free slots on or after from (a date), inside the horizon.
func (s *Server) freeSlots(from time.Time, limit int) []Slot {
	now := s.now()
	day := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	if min := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC); day.Before(min) {
		day = min
	}
	var out []Slot
	for d := 0; d <= horizonDays && len(out) < limit; d++ {
		date := day.AddDate(0, 0, d)
		if date.Weekday() == time.Saturday || date.Weekday() == time.Sunday {
			continue
		}
		for _, hm := range slotTimes {
			id := date.Format("2006-01-02") + "T" + hm
			if _, err := s.slotTime(id); err != nil || s.taken(id, "") {
				continue
			}
			t, _ := time.Parse(slotLayout, id)
			out = append(out, Slot{ID: id, Label: t.Format("Monday 2 January, 15:04")})
			if len(out) == limit {
				break
			}
		}
	}
	return out
}

func (s *Server) addAppointment(patientID, slotID, kind string) (Appointment, error) {
	t, err := s.slotTime(slotID)
	if err != nil {
		return Appointment{}, err
	}
	if !kinds[kind] {
		return Appointment{}, errors.New("kind must be checkup, cleaning, filling or consultation")
	}
	if s.taken(slotID, "") {
		return Appointment{}, fmt.Errorf("slot %s is already taken; call find_slots for another", slotID)
	}
	if len(s.appts) >= MaxAppointments {
		return Appointment{}, errors.New("the schedule is full")
	}
	a := Appointment{ID: "a" + strconv.Itoa(s.nextA), PatientID: patientID, Kind: kind, SlotID: slotID, When: t.Format("Monday 2 January, 15:04")}
	s.nextA++
	s.appts = append(s.appts, a)
	return a, nil
}

func (s *Server) patient(id string) (Patient, bool) {
	for _, p := range s.patients {
		if p.ID == id {
			return p, true
		}
	}
	return Patient{}, false
}

type (
	findIn struct {
		Name  string `json:"name"`
		Phone string `json:"phone,omitempty"`
	}
	findOut struct {
		Found   bool     `json:"found"`
		Patient *Patient `json:"patient,omitempty"`
	}
	registerIn struct {
		Name  string `json:"name"`
		Phone string `json:"phone"`
	}
	patientOut struct {
		Patient Patient `json:"patient"`
	}
	patientIDIn struct {
		PatientID string `json:"patient_id"`
	}
	listOut struct {
		Appointments []Appointment `json:"appointments"`
	}
	slotsIn struct {
		FromDate string `json:"from_date,omitempty"`
	}
	slotsOut struct {
		Slots []Slot `json:"slots"`
	}
	bookIn struct {
		PatientID string `json:"patient_id"`
		SlotID    string `json:"slot_id"`
		Kind      string `json:"kind"`
	}
	apptOut struct {
		Appointment Appointment `json:"appointment"`
	}
	moveIn struct {
		AppointmentID string `json:"appointment_id"`
		SlotID        string `json:"slot_id"`
	}
	cancelIn struct {
		AppointmentID string `json:"appointment_id"`
	}
	cancelOut struct {
		Cancelled string `json:"cancelled"`
	}
)

func (s *Server) addTools() {
	mcp.AddTool(s.srv, &mcp.Tool{Name: "find_patient", Description: "Find an existing patient by full name (and optionally phone). found is false for a new patient."},
		func(_ context.Context, _ *mcp.CallToolRequest, in findIn) (*mcp.CallToolResult, findOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, p := range s.patients {
				if norm(p.Name) == norm(in.Name) && (in.Phone == "" || digits(p.Phone) == digits(in.Phone)) {
					return nil, findOut{Found: true, Patient: &p}, nil
				}
			}
			return nil, findOut{}, nil
		})
	mcp.AddTool(s.srv, &mcp.Tool{Name: "register_patient", Description: "Register a new patient with full name and phone number. Returns the patient id."},
		func(_ context.Context, _ *mcp.CallToolRequest, in registerIn) (*mcp.CallToolResult, patientOut, error) {
			name, phone := strings.Join(strings.Fields(in.Name), " "), strings.TrimSpace(in.Phone)
			if name == "" || phone == "" || len(name) > maxField || len(phone) > maxField {
				return nil, patientOut{}, errors.New("name and phone are required (at most 80 characters each)")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, p := range s.patients {
				if norm(p.Name) == norm(name) && digits(p.Phone) == digits(phone) {
					return nil, patientOut{Patient: p}, nil // registering twice returns the same patient
				}
			}
			if len(s.patients) >= MaxPatients {
				return nil, patientOut{}, errors.New("the patient register is full")
			}
			return nil, patientOut{Patient: s.addPatient(name, phone)}, nil
		})
	mcp.AddTool(s.srv, &mcp.Tool{Name: "list_appointments", Description: "List a patient's upcoming appointments."},
		func(_ context.Context, _ *mcp.CallToolRequest, in patientIDIn) (*mcp.CallToolResult, listOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			out := listOut{Appointments: []Appointment{}}
			for _, a := range s.appts {
				if a.PatientID == in.PatientID {
					out.Appointments = append(out.Appointments, a)
				}
			}
			return nil, out, nil
		})
	mcp.AddTool(s.srv, &mcp.Tool{Name: "find_slots", Description: "List up to six free appointment slots, starting from_date (YYYY-MM-DD, default today). Offer at most three to the caller."},
		func(_ context.Context, _ *mcp.CallToolRequest, in slotsIn) (*mcp.CallToolResult, slotsOut, error) {
			from := s.now()
			if in.FromDate != "" {
				d, err := time.Parse("2006-01-02", in.FromDate)
				if err != nil {
					return nil, slotsOut{}, errors.New("from_date must be YYYY-MM-DD")
				}
				from = d
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			return nil, slotsOut{Slots: s.freeSlots(from, slotsPerReply)}, nil
		})
	mcp.AddTool(s.srv, &mcp.Tool{Name: "book_appointment", Description: "Book a slot (id from find_slots) for a patient. kind is checkup, cleaning, filling or consultation."},
		func(_ context.Context, _ *mcp.CallToolRequest, in bookIn) (*mcp.CallToolResult, apptOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if _, ok := s.patient(in.PatientID); !ok {
				return nil, apptOut{}, errors.New("unknown patient_id; register the patient first")
			}
			a, err := s.addAppointment(in.PatientID, in.SlotID, in.Kind)
			return nil, apptOut{Appointment: a}, err
		})
	mcp.AddTool(s.srv, &mcp.Tool{Name: "reschedule_appointment", Description: "Move an appointment to another free slot (id from find_slots)."},
		func(_ context.Context, _ *mcp.CallToolRequest, in moveIn) (*mcp.CallToolResult, apptOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			for i, a := range s.appts {
				if a.ID != in.AppointmentID {
					continue
				}
				t, err := s.slotTime(in.SlotID)
				if err != nil {
					return nil, apptOut{}, err
				}
				if s.taken(in.SlotID, a.ID) {
					return nil, apptOut{}, fmt.Errorf("slot %s is already taken; call find_slots for another", in.SlotID)
				}
				s.appts[i].SlotID, s.appts[i].When = in.SlotID, t.Format("Monday 2 January, 15:04")
				return nil, apptOut{Appointment: s.appts[i]}, nil
			}
			return nil, apptOut{}, errors.New("unknown appointment_id")
		})
	mcp.AddTool(s.srv, &mcp.Tool{Name: "cancel_appointment", Description: "Cancel an appointment. Confirm with the caller before calling."},
		func(_ context.Context, _ *mcp.CallToolRequest, in cancelIn) (*mcp.CallToolResult, cancelOut, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			for i, a := range s.appts {
				if a.ID == in.AppointmentID {
					s.appts = append(s.appts[:i], s.appts[i+1:]...)
					return nil, cancelOut{Cancelled: a.ID}, nil
				}
			}
			return nil, cancelOut{}, errors.New("unknown appointment_id")
		})
}
