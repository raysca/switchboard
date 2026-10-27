package atlasmcp

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cities are the five cities the fixtures cover; two distinct ones make a flight or transfer,
// one of them makes a stay.
var cities = []string{"London", "Paris", "New York", "Dubai", "Singapore"}

// cityCountry is the country each city belongs to, so the advisory tool accepts either.
var cityCountry = map[string]string{
	"London":    "United Kingdom",
	"Paris":     "France",
	"New York":  "United States",
	"Dubai":     "United Arab Emirates",
	"Singapore": "Singapore",
}

var countries = []string{"United Kingdom", "France", "United States", "United Arab Emirates", "Singapore"}

// countryAliases are the short forms a caller may reasonably use for a country.
var countryAliases = map[string]string{
	"UK":  "United Kingdom",
	"USA": "United States",
	"US":  "United States",
	"UAE": "United Arab Emirates",
}

// weather is the fixed forecast of one city. The demo weather never changes; only the date does.
type weather struct {
	Condition  string
	HighC      int
	LowC       int
	RainChance int
}

var weatherByCity = map[string]weather{
	"London":    {"Light rain", 15, 9, 60},
	"Paris":     {"Partly cloudy", 18, 11, 25},
	"New York":  {"Sunny", 22, 14, 10},
	"Dubai":     {"Hot and sunny", 36, 27, 0},
	"Singapore": {"Thunderstorms", 31, 26, 80},
}

// advisory is the fixed travel advisory of one country.
type advisory struct {
	Level   string
	Summary string
}

var advisoryByCountry = map[string]advisory{
	"United Kingdom":       {"low", "No travel restrictions. Carry standard documents and check public transport updates."},
	"France":               {"low", "No travel restrictions. Rail services can be affected by industrial action; check schedules."},
	"United States":        {"low", "No travel restrictions. Visa-waiver visitors need an approved ESTA before departure."},
	"United Arab Emirates": {"moderate", "No travel restrictions. Dress modestly in public places and follow local laws."},
	"Singapore":            {"low", "No travel restrictions. Strict local laws apply, including on littering."},
}

var (
	airlines     = []string{"Atlas Air", "Meridian Airways", "Northwind"}
	departTimes  = []string{"07:15", "12:40", "18:05"}
	baseMinutes  = []int{150, 215, 380}
	hotelRatings = []float64{4.3, 4.7}
	vehicles     = []string{"Sedan", "SUV"}
)

// hotelNames gives each city two fixed hotels; the index selects one.
var hotelNames = map[string][]string{
	"London":    {"The Kensington Atlas", "Riverlight Hotel"},
	"Paris":     {"Hotel Lumiere", "Rive Gauche Suites"},
	"New York":  {"Hudson Line Hotel", "Midtown Atlas"},
	"Dubai":     {"Marina Skyline", "Dune Palace"},
	"Singapore": {"Orchard Atlas", "Marina Bay House"},
}

// canonicalCity returns the fixture spelling of a city name, whatever the case or spacing.
func canonicalCity(name string) (string, bool) {
	v := strings.Join(strings.Fields(name), " ")
	for _, city := range cities {
		if strings.EqualFold(v, city) {
			return city, true
		}
	}
	return "", false
}

// canonicalCountry returns the fixture spelling of a country name; a city name is accepted too,
// because a caller often says the city when asked about a country. The bool is false when unknown.
func canonicalCountry(name string) (string, bool) {
	v := strings.Join(strings.Fields(name), " ")
	for _, country := range countries {
		if strings.EqualFold(v, country) {
			return country, true
		}
	}
	for short, country := range countryAliases {
		if strings.EqualFold(v, short) {
			return country, true
		}
	}
	if city, ok := canonicalCity(v); ok {
		return cityCountry[city], true
	}
	return "", false
}

// cityIndex is the position of a canonical city, used to make fixture prices differ per city pair.
func cityIndex(city string) int {
	for i, c := range cities {
		if c == city {
			return i
		}
	}
	return 0
}

// digits keeps only the digits of a phone number, so "555-0142" and "555 0142" compare equal.
func digits(v string) string {
	var b strings.Builder
	b.Grow(len(v))
	for _, r := range v {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// slug turns a city name into the lower-case form used inside fixture ids.
func slug(city string) string { return strings.ToLower(strings.ReplaceAll(city, " ", "-")) }

// addMinutes shifts a 15:04 clock by mins and returns the new 15:04 clock.
func addMinutes(clock string, mins int) string {
	t, err := time.Parse("15:04", clock)
	if err != nil {
		return clock
	}
	return t.Add(time.Duration(mins) * time.Minute).Format("15:04")
}

func legPrice(from, to string, n int) int {
	return 18000 + cityIndex(from)*2100 + cityIndex(to)*3300 + n*5400
}

func stayPerNight(city string, n int) int {
	return 16500 + cityIndex(city)*3800 + n*7200
}

func transferPrice(from, to string, n int) int {
	return 4200 + cityIndex(from)*650 + cityIndex(to)*850 + n*2600
}

// reference is the five-character supplier reference of a fixture record, derived from its number.
func reference(n int) string {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	var b strings.Builder
	b.Grow(len("AT-") + 5)
	b.WriteString("AT-")
	v := n*7919 + 104729
	for i := 0; i < 5; i++ {
		b.WriteByte(alphabet[v%len(alphabet)])
		v /= len(alphabet)
	}
	return b.String()
}

// flightsBetween lists the fixed daily options between two cities.
func flightsBetween(from, to, date string) []Flight {
	out := make([]Flight, 0, len(airlines))
	for n := range airlines {
		out = append(out, Flight{
			ID:         flightID(from, to, date, n),
			From:       from,
			To:         to,
			Date:       date,
			Depart:     departTimes[n],
			Arrive:     addMinutes(departTimes[n], baseMinutes[n]+20*(cityIndex(from)+cityIndex(to))),
			Airline:    airlines[n],
			Stops:      n % 2,
			PriceCents: legPrice(from, to, n),
			Currency:   currency,
		})
	}
	return out
}

// staysIn lists the fixed hotels of one city for a stay of nights nights.
func staysIn(city, checkIn, checkOut string, nights int) []Stay {
	names := hotelNames[city]
	out := make([]Stay, 0, len(names))
	for n, name := range names {
		perNight := stayPerNight(city, n)
		out = append(out, Stay{
			ID:                 stayID(city, checkIn, checkOut, n),
			Name:               name,
			City:               city,
			CheckIn:            checkIn,
			CheckOut:           checkOut,
			Nights:             nights,
			PricePerNightCents: perNight,
			TotalCents:         perNight * nights,
			Rating:             hotelRatings[n],
			Currency:           currency,
		})
	}
	return out
}

// transfersBetween lists the fixed ground options between two cities.
func transfersBetween(from, to, date string) []Transfer {
	out := make([]Transfer, 0, len(vehicles))
	for n, vehicle := range vehicles {
		out = append(out, Transfer{
			ID:         transferID(from, to, date, n),
			From:       from,
			To:         to,
			Date:       date,
			Vehicle:    vehicle,
			PriceCents: transferPrice(from, to, n),
			Currency:   currency,
		})
	}
	return out
}

// flightID names one flight option: two cities, a day and an option number.
func flightID(from, to, date string, n int) string {
	return "fl-" + slug(from) + "-" + slug(to) + "-" + date + "-" + strconv.Itoa(n+1)
}

// transferID names one transfer option in the same shape as a flight id.
func transferID(from, to, date string, n int) string {
	return "tr-" + slug(from) + "-" + slug(to) + "-" + date + "-" + strconv.Itoa(n+1)
}

// stayID names one stay option: a city, the check-in and check-out days and an option number.
func stayID(city, checkIn, checkOut string, n int) string {
	return "st-" + slug(city) + "-" + checkIn + "-" + checkOut + "-" + strconv.Itoa(n+1)
}

// splitPairID separates a generated two-city id into its endpoints, day and option number.
// optionCount bounds the option number so an id for some other tool cannot be read as a flight.
func splitPairID(prefix, id string, optionCount int) (from, to, date string, n int, ok bool) {
	rest, ok := strings.CutPrefix(id, prefix)
	if !ok {
		return "", "", "", 0, false
	}
	for _, f := range cities {
		afterFrom, hit := strings.CutPrefix(rest, slug(f)+"-")
		if !hit {
			continue
		}
		for _, t := range cities {
			afterTo, hit := strings.CutPrefix(afterFrom, slug(t)+"-")
			if !hit {
				continue
			}
			parts := strings.Split(afterTo, "-")
			if len(parts) != 4 {
				continue
			}
			num, err := strconv.Atoi(parts[3])
			if err != nil || num < 1 || num > optionCount {
				continue
			}
			return f, t, strings.Join(parts[:3], "-"), num, true
		}
	}
	return "", "", "", 0, false
}

// splitStayID separates a generated stay id into its city, both days and option number.
func splitStayID(id string, optionCount int) (city, checkIn, checkOut string, n int, ok bool) {
	rest, ok := strings.CutPrefix(id, "st-")
	if !ok {
		return "", "", "", 0, false
	}
	for _, candidate := range cities {
		afterCity, hit := strings.CutPrefix(rest, slug(candidate)+"-")
		if !hit {
			continue
		}
		parts := strings.Split(afterCity, "-")
		if len(parts) != 7 {
			continue
		}
		num, err := strconv.Atoi(parts[6])
		if err != nil || num < 1 || num > optionCount {
			continue
		}
		return candidate, strings.Join(parts[0:3], "-"), strings.Join(parts[3:6], "-"), num, true
	}
	return "", "", "", 0, false
}

// today is the current fixture day at midnight UTC, derived from the injected clock.
func (s *Server) today() time.Time {
	now := s.now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// inWindow reports whether day is a real day inside the fixture booking window.
func (s *Server) inWindow(day time.Time) bool {
	today := s.today()
	return !day.Before(today) && !day.After(today.AddDate(0, 0, horizonDays))
}

// parseDay parses a YYYY-MM-DD day and checks it is inside the booking window.
func (s *Server) parseDay(raw string) (time.Time, error) {
	raw = ref(raw)
	day, err := time.Parse(dateLayout, raw)
	if err != nil || day.Format(dateLayout) != raw {
		return time.Time{}, fmt.Errorf("date %q must be YYYY-MM-DD", raw)
	}
	if !s.inWindow(day) {
		return time.Time{}, fmt.Errorf("date %s is outside the %d-day booking window", raw, horizonDays)
	}
	return day, nil
}

// dayOrDefault parses raw, or returns tomorrow when raw is empty.
func (s *Server) dayOrDefault(raw string) (time.Time, error) {
	if raw == "" {
		raw = s.now().AddDate(0, 0, 1).Format(dateLayout)
	}
	return s.parseDay(raw)
}

// stayDates validates the two ends of a stay: check-in defaults to tomorrow, check-out to two
// nights later. It returns the canonical days and the night count.
func (s *Server) stayDates(checkInRaw, checkOutRaw string) (checkIn, checkOut string, nights int, err error) {
	in, err := s.dayOrDefault(checkInRaw)
	if err != nil {
		return "", "", 0, err
	}
	var out time.Time
	if checkOutRaw == "" {
		out = in.AddDate(0, 0, 2)
		if !s.inWindow(out) {
			return "", "", 0, fmt.Errorf("a stay from %s must end by the last bookable day", in.Format(dateLayout))
		}
	} else if out, err = s.parseDay(checkOutRaw); err != nil {
		return "", "", 0, err
	}
	nights = int(out.Sub(in).Hours() / 24)
	if nights < 1 {
		return "", "", 0, fmt.Errorf("check_out %s must be after check_in %s", out.Format(dateLayout), in.Format(dateLayout))
	}
	if nights > horizonDays {
		return "", "", 0, fmt.Errorf("a stay may last at most %d nights", horizonDays)
	}
	return in.Format(dateLayout), out.Format(dateLayout), nights, nil
}

// flightByID returns the flight a generated id names, validating the id and its day.
func (s *Server) flightByID(id string) (Flight, error) {
	from, to, date, n, ok := splitPairID("fl-", id, len(airlines))
	if !ok {
		return Flight{}, fmt.Errorf("%q is not a flight id; use an id from search_flights", id)
	}
	if from == to {
		return Flight{}, fmt.Errorf("%q is not a flight id; a flight needs two different cities", id)
	}
	if _, err := s.parseDay(date); err != nil {
		return Flight{}, err
	}
	return flightsBetween(from, to, date)[n-1], nil
}

// transferByID returns the transfer a generated id names, validating the id and its day.
func (s *Server) transferByID(id string) (Transfer, error) {
	from, to, date, n, ok := splitPairID("tr-", id, len(vehicles))
	if !ok {
		return Transfer{}, fmt.Errorf("%q is not a transfer id; use an id from search_transfers", id)
	}
	if from == to {
		return Transfer{}, fmt.Errorf("%q is not a transfer id; a transfer needs two different cities", id)
	}
	if _, err := s.parseDay(date); err != nil {
		return Transfer{}, err
	}
	return transfersBetween(from, to, date)[n-1], nil
}

// stayByID returns the stay a generated id names, validating the id and its days.
func (s *Server) stayByID(id string) (Stay, error) {
	city, checkIn, checkOut, n, ok := splitStayID(id, len(hotelRatings))
	if !ok {
		return Stay{}, fmt.Errorf("%q is not a stay id; use an id from search_stays", id)
	}
	canonicalIn, canonicalOut, nights, err := s.stayDates(checkIn, checkOut)
	if err != nil {
		return Stay{}, err
	}
	return staysIn(city, canonicalIn, canonicalOut, nights)[n-1], nil
}
