package dentalmcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"govoice/server/mcp"
	"govoice/server/mcp/dentalmcp"
)

// Monday 12 October 2026, 08:00 UTC.
var monday = time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)

func start(t *testing.T) (call func(tool, args string) (map[string]any, error), base string) {
	t.Helper()
	srv, err := dentalmcp.New(dentalmcp.Options{Token: "tok", Now: func() time.Time { return monday }})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	c := mcp.NewSDKClient(mcp.AllowLoopbackHTTP())
	return func(tool, args string) (map[string]any, error) {
		res, err := c.CallTool(context.Background(), ts.URL+"/mcp", "tok", tool, json.RawMessage(args))
		if err != nil {
			return nil, err
		}
		var out map[string]any
		return out, json.Unmarshal(res.JSON, &out)
	}, ts.URL
}

func TestSeededPatientIsFoundWhateverTheCaseAndSpacing(t *testing.T) {
	call, _ := start(t)
	out, err := call("find_patient", `{"name":"  maria   LOPEZ "}`)
	if err != nil || out["found"] != true {
		t.Fatalf("out=%v err=%v", out, err)
	}
	if out, _ = call("find_patient", `{"name":"Nobody Here"}`); out["found"] != false {
		t.Fatalf("unknown patient: %v", out)
	}
}

func TestRegisterThenBookRescheduleCancel(t *testing.T) {
	call, _ := start(t)
	reg, err := call("register_patient", `{"name":"Sam Taylor","phone":"555 0100"}`)
	if err != nil {
		t.Fatal(err)
	}
	pid := reg["patient"].(map[string]any)["id"].(string)

	slots, _ := call("find_slots", `{}`)
	list := slots["slots"].([]any)
	if len(list) == 0 || len(list) > 6 {
		t.Fatalf("slots = %v", slots)
	}
	first := list[0].(map[string]any)["id"].(string)
	second := list[1].(map[string]any)["id"].(string)

	booked, err := call("book_appointment", `{"patient_id":"`+pid+`","slot_id":"`+first+`","kind":"checkup"}`)
	if err != nil {
		t.Fatal(err)
	}
	aid := booked["appointment"].(map[string]any)["id"].(string)

	// The slot is gone for the next caller.
	other, _ := call("register_patient", `{"name":"Lee Wong","phone":"555 0101"}`)
	opid := other["patient"].(map[string]any)["id"].(string)
	if _, err := call("book_appointment", `{"patient_id":"`+opid+`","slot_id":"`+first+`","kind":"checkup"}`); err == nil {
		t.Fatal("a taken slot must not be booked twice")
	}

	if _, err := call("reschedule_appointment", `{"appointment_id":"`+aid+`","slot_id":"`+second+`"}`); err != nil {
		t.Fatal(err)
	}
	// Moving frees the old slot.
	if _, err := call("book_appointment", `{"patient_id":"`+opid+`","slot_id":"`+first+`","kind":"checkup"}`); err != nil {
		t.Fatalf("the old slot is free after a reschedule: %v", err)
	}
	if _, err := call("cancel_appointment", `{"appointment_id":"`+aid+`"}`); err != nil {
		t.Fatal(err)
	}
	apps, _ := call("list_appointments", `{"patient_id":"`+pid+`"}`)
	if got := apps["appointments"]; got != nil && len(got.([]any)) != 0 {
		t.Fatalf("appointments after cancel = %v", got)
	}
}

func TestSlotsAreWeekdaysInsideTheHorizon(t *testing.T) {
	call, _ := start(t)
	out, _ := call("find_slots", `{"from_date":"2026-10-16"}`) // a Friday
	for _, s := range out["slots"].([]any) {
		id := s.(map[string]any)["id"].(string)
		d, err := time.Parse("2006-01-02T15:04", id)
		if err != nil || d.Weekday() == time.Saturday || d.Weekday() == time.Sunday || d.Before(monday) || d.After(monday.AddDate(0, 0, 14)) {
			t.Fatalf("slot %q is outside the clinic's week", id)
		}
	}
	if _, err := call("book_appointment", `{"patient_id":"p1","slot_id":"2026-10-17T09:00","kind":"checkup"}`); err == nil { // Saturday
		t.Fatal("a weekend slot must not be bookable")
	}
}

func TestTheRegistryIsBounded(t *testing.T) {
	call, _ := start(t)
	var err error
	for i := 0; i < dentalmcp.MaxPatients+1 && err == nil; i++ {
		_, err = call("register_patient", `{"name":"Patient `+strconv.Itoa(i)+`","phone":"5`+strconv.Itoa(i)+`"}`)
	}
	if err == nil {
		t.Fatal("registering beyond MaxPatients must fail")
	}
}

func TestTokenIsRequiredOnMCPNotOnHealth(t *testing.T) {
	_, base := start(t)
	c := mcp.NewSDKClient(mcp.AllowLoopbackHTTP())
	if _, err := c.ListTools(context.Background(), base+"/mcp", "wrong"); !errors.Is(err, mcp.ErrAuthenticationFailed) {
		t.Fatalf("wrong token: %v", err)
	}
	resp, err := http.Get(base + "/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("health: %v %v", resp, err)
	}
	resp.Body.Close()
	if _, err := dentalmcp.New(dentalmcp.Options{}); err == nil {
		t.Fatal("a server without a token must not start")
	}
}

func TestNonCanonicalSlotIdsCannotDoubleBook(t *testing.T) {
	call, _ := start(t)
	a, _ := call("register_patient", `{"name":"Ann One","phone":"555 0200"}`)
	b, _ := call("register_patient", `{"name":"Bob Two","phone":"555 0201"}`)
	apid := a["patient"].(map[string]any)["id"].(string)
	bpid := b["patient"].(map[string]any)["id"].(string)
	_, err := call("book_appointment", `{"patient_id":"`+apid+`","slot_id":"2026-10-19T09:00","kind":"checkup"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call("book_appointment", `{"patient_id":"`+bpid+`","slot_id":"2026-10-19T9:00","kind":"checkup"}`); err == nil {
		t.Fatal("a non-canonical id must not double-book a taken slot")
	}
	bk, err := call("book_appointment", `{"patient_id":"`+bpid+`","slot_id":"2026-10-19T11:00","kind":"checkup"}`)
	if err != nil {
		t.Fatal(err)
	}
	bid := bk["appointment"].(map[string]any)["id"].(string)
	if _, err := call("reschedule_appointment", `{"appointment_id":"`+bid+`","slot_id":"2026-10-19T9:00"}`); err == nil {
		t.Fatal("reschedule onto a taken slot via a non-canonical id must fail")
	}
}

// Past appointments are pruned, so list_appointments means upcoming and history never fills the schedule.
func TestPastAppointmentsDisappear(t *testing.T) {
	var clock atomic.Pointer[time.Time]
	clock.Store(&monday)
	srv, err := dentalmcp.New(dentalmcp.Options{Token: "tok", Now: func() time.Time { return *clock.Load() }})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	c := mcp.NewSDKClient(mcp.AllowLoopbackHTTP())
	call := func(tool, args string) map[string]any {
		t.Helper()
		res, err := c.CallTool(context.Background(), ts.URL+"/mcp", "tok", tool, json.RawMessage(args))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		_ = json.Unmarshal(res.JSON, &out)
		return out
	}
	pid := call("find_patient", `{"name":"Maria Lopez"}`)["patient"].(map[string]any)["id"].(string)
	if n := len(call("list_appointments", `{"patient_id":"`+pid+`"}`)["appointments"].([]any)); n != 1 {
		t.Fatalf("seeded appointments = %d, want 1", n)
	}
	later := monday.AddDate(0, 0, 9) // past every seeded slot
	clock.Store(&later)
	if n := len(call("list_appointments", `{"patient_id":"`+pid+`"}`)["appointments"].([]any)); n != 0 {
		t.Fatalf("a past appointment is still listed: %d", n)
	}
}
