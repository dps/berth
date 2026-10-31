package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func init() {
	register(
		Provider{Name: "weatherapi", Title: "WeatherAPI", SignUp: "https://www.weatherapi.com/signup.aspx"},
		Provider{Name: "openmeteo", Title: "Open-Meteo", Note: "free for non-commercial use, no key; add weatherapi for anything else"},
	)
	location := Field{Name: "location", Type: "string", Required: true, Desc: "A city like \"Paris\" or \"San Francisco, CA\", or \"lat,lon\"."}
	units := Field{Name: "units", Type: "string", Enum: []string{"metric", "imperial"}, Desc: "Default metric: °C, km/h, mm. Imperial: °F, mph, inches."}
	place := Field{Name: "location", Type: "object", Fields: []Field{
		{Name: "name", Type: "string"}, {Name: "region", Type: "string"}, {Name: "country", Type: "string"},
		{Name: "lat", Type: "number"}, {Name: "lon", Type: "number"},
	}}
	define(&Tool{
		Name:    "weather.current",
		Summary: "Current conditions for a place.",
		Input:   []Field{location, units},
		Output: []Field{place, {Name: "units", Type: "string"}, {Name: "condition", Type: "string"},
			{Name: "temperature", Type: "number"}, {Name: "feels_like", Type: "number"}, {Name: "humidity", Type: "number", Desc: "Percent."},
			{Name: "wind_speed", Type: "number"}, {Name: "precipitation", Type: "number"}},
		Cache:   10 * time.Minute,
		Example: `{"location":"Paris"}`,
		Order:   []string{"weatherapi", "openmeteo"},
		Run:     map[string]RunFunc{"weatherapi": weatherAPICurrent, "openmeteo": openMeteoCurrent},
	})
	define(&Tool{
		Name:    "weather.forecast",
		Summary: "Daily forecast for a place.",
		Input:   []Field{location, units, {Name: "days", Type: "integer", Min: 1, Max: 14, Desc: "Days ahead, default 3. Free plans may return fewer."}},
		Output: []Field{place, {Name: "units", Type: "string"}, {Name: "days", Type: "array", Items: &Field{Type: "object", Fields: []Field{
			{Name: "date", Type: "string"}, {Name: "condition", Type: "string"}, {Name: "high", Type: "number"}, {Name: "low", Type: "number"},
			{Name: "precipitation_chance", Type: "number", Desc: "Percent."},
		}}}},
		Cache: 30 * time.Minute,
		Order: []string{"weatherapi", "openmeteo"},
		Run:   map[string]RunFunc{"weatherapi": weatherAPIForecast, "openmeteo": openMeteoForecast},
	})
}

type place struct {
	Name    string  `json:"name"`
	Region  string  `json:"region,omitempty"`
	Country string  `json:"country,omitempty"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
}

type weatherNow struct {
	Location      place   `json:"location"`
	Units         string  `json:"units"`
	Condition     string  `json:"condition"`
	Temperature   float64 `json:"temperature"`
	FeelsLike     float64 `json:"feels_like"`
	Humidity      float64 `json:"humidity"`
	WindSpeed     float64 `json:"wind_speed"`
	Precipitation float64 `json:"precipitation"`
}

type weatherDay struct {
	Date                string  `json:"date"`
	Condition           string  `json:"condition"`
	High                float64 `json:"high"`
	Low                 float64 `json:"low"`
	PrecipitationChance float64 `json:"precipitation_chance"`
}

type weatherDays struct {
	Location place        `json:"location"`
	Units    string       `json:"units"`
	Days     []weatherDay `json:"days"`
}

func unitsOf(args map[string]any) string {
	if str(args, "units") == "imperial" {
		return "imperial"
	}
	return "metric"
}

// ---- WeatherAPI ----

type wapiCond struct {
	Text string `json:"text"`
}

type wapiPlace struct {
	Name    string  `json:"name"`
	Region  string  `json:"region"`
	Country string  `json:"country"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
}

func (p wapiPlace) place() place { return place(p) }

// wapiGet asks WeatherAPI, which answers 400 for a place it can't find.
func wapiGet(ctx context.Context, c *Call, path string, q url.Values, into any) error {
	q.Set("key", c.Key)
	err := c.get(ctx, "https://api.weatherapi.com/v1/"+path+"?"+q.Encode(), into)
	if se, ok := err.(*statusError); ok && se.Status == http.StatusBadRequest {
		return notFound("no weather for %q", q.Get("q"))
	}
	return err
}

func weatherAPICurrent(ctx context.Context, c *Call, args map[string]any) (any, error) {
	var r struct {
		Location wapiPlace `json:"location"`
		Current  struct {
			Condition wapiCond `json:"condition"`
			TempC     float64  `json:"temp_c"`
			TempF     float64  `json:"temp_f"`
			FeelsC    float64  `json:"feelslike_c"`
			FeelsF    float64  `json:"feelslike_f"`
			Humidity  float64  `json:"humidity"`
			WindKph   float64  `json:"wind_kph"`
			WindMph   float64  `json:"wind_mph"`
			PrecipMm  float64  `json:"precip_mm"`
			PrecipIn  float64  `json:"precip_in"`
		} `json:"current"`
	}
	if err := wapiGet(ctx, c, "current.json", url.Values{"q": {str(args, "location")}}, &r); err != nil {
		return nil, err
	}
	w := r.Current
	out := weatherNow{Location: r.Location.place(), Units: unitsOf(args), Condition: w.Condition.Text, Humidity: w.Humidity}
	if out.Units == "imperial" {
		out.Temperature, out.FeelsLike, out.WindSpeed, out.Precipitation = w.TempF, w.FeelsF, w.WindMph, w.PrecipIn
	} else {
		out.Temperature, out.FeelsLike, out.WindSpeed, out.Precipitation = w.TempC, w.FeelsC, w.WindKph, w.PrecipMm
	}
	return out, nil
}

func weatherAPIForecast(ctx context.Context, c *Call, args map[string]any) (any, error) {
	var r struct {
		Location wapiPlace `json:"location"`
		Forecast struct {
			Days []struct {
				Date string `json:"date"`
				Day  struct {
					Condition wapiCond `json:"condition"`
					MaxC      float64  `json:"maxtemp_c"`
					MaxF      float64  `json:"maxtemp_f"`
					MinC      float64  `json:"mintemp_c"`
					MinF      float64  `json:"mintemp_f"`
					Rain      float64  `json:"daily_chance_of_rain"`
				} `json:"day"`
			} `json:"forecastday"`
		} `json:"forecast"`
	}
	q := url.Values{"q": {str(args, "location")}, "days": {strconv.Itoa(intArg(args, "days", 3))}}
	if err := wapiGet(ctx, c, "forecast.json", q, &r); err != nil {
		return nil, err
	}
	out := weatherDays{Location: r.Location.place(), Units: unitsOf(args), Days: []weatherDay{}}
	for _, d := range r.Forecast.Days {
		w := weatherDay{Date: d.Date, Condition: d.Day.Condition.Text, High: d.Day.MaxC, Low: d.Day.MinC, PrecipitationChance: d.Day.Rain}
		if out.Units == "imperial" {
			w.High, w.Low = d.Day.MaxF, d.Day.MinF
		}
		out.Days = append(out.Days, w)
	}
	return out, nil
}

// ---- Open-Meteo ----

var latLon = regexp.MustCompile(`^\s*(-?\d+(?:\.\d+)?)\s*,\s*(-?\d+(?:\.\d+)?)\s*$`)

// openMeteoPlace turns a location into coordinates. Its geocoder takes a
// place name only, so "Paris, France" is retried as "Paris".
func openMeteoPlace(ctx context.Context, c *Call, location string) (place, error) {
	if m := latLon.FindStringSubmatch(location); m != nil {
		lat, _ := strconv.ParseFloat(m[1], 64)
		lon, _ := strconv.ParseFloat(m[2], 64)
		return place{Name: strings.TrimSpace(location), Lat: lat, Lon: lon}, nil
	}
	names := []string{location}
	if first, _, ok := strings.Cut(location, ","); ok {
		names = append(names, strings.TrimSpace(first))
	}
	for _, name := range names {
		var r struct {
			Results []struct {
				Name      string  `json:"name"`
				Admin1    string  `json:"admin1"`
				Country   string  `json:"country"`
				Latitude  float64 `json:"latitude"`
				Longitude float64 `json:"longitude"`
			} `json:"results"`
		}
		q := url.Values{"name": {name}, "count": {"1"}, "language": {"en"}, "format": {"json"}}
		if err := c.get(ctx, "https://geocoding-api.open-meteo.com/v1/search?"+q.Encode(), &r); err != nil {
			return place{}, err
		}
		if len(r.Results) > 0 {
			p := r.Results[0]
			return place{p.Name, p.Admin1, p.Country, p.Latitude, p.Longitude}, nil
		}
	}
	return place{}, notFound("no place called %q", location)
}

func openMeteoQuery(p place, units string) url.Values {
	q := url.Values{"latitude": {fmt.Sprint(p.Lat)}, "longitude": {fmt.Sprint(p.Lon)}, "timezone": {"auto"}}
	if units == "imperial" {
		q.Set("temperature_unit", "fahrenheit")
		q.Set("wind_speed_unit", "mph")
		q.Set("precipitation_unit", "inch")
	}
	return q
}

func openMeteoCurrent(ctx context.Context, c *Call, args map[string]any) (any, error) {
	p, err := openMeteoPlace(ctx, c, str(args, "location"))
	if err != nil {
		return nil, err
	}
	units := unitsOf(args)
	q := openMeteoQuery(p, units)
	q.Set("current", "temperature_2m,relative_humidity_2m,apparent_temperature,precipitation,weather_code,wind_speed_10m")
	var r struct {
		Current struct {
			Temp     float64 `json:"temperature_2m"`
			Humidity float64 `json:"relative_humidity_2m"`
			Feels    float64 `json:"apparent_temperature"`
			Precip   float64 `json:"precipitation"`
			Code     int     `json:"weather_code"`
			Wind     float64 `json:"wind_speed_10m"`
		} `json:"current"`
	}
	if err := c.get(ctx, "https://api.open-meteo.com/v1/forecast?"+q.Encode(), &r); err != nil {
		return nil, err
	}
	w := r.Current
	return weatherNow{p, units, wmoText(w.Code), w.Temp, w.Feels, w.Humidity, w.Wind, w.Precip}, nil
}

func openMeteoForecast(ctx context.Context, c *Call, args map[string]any) (any, error) {
	p, err := openMeteoPlace(ctx, c, str(args, "location"))
	if err != nil {
		return nil, err
	}
	units := unitsOf(args)
	q := openMeteoQuery(p, units)
	q.Set("daily", "weather_code,temperature_2m_max,temperature_2m_min,precipitation_probability_max")
	q.Set("forecast_days", strconv.Itoa(intArg(args, "days", 3)))
	var r struct {
		Daily struct {
			Time []string   `json:"time"`
			Code []int      `json:"weather_code"`
			Max  []float64  `json:"temperature_2m_max"`
			Min  []float64  `json:"temperature_2m_min"`
			Rain []*float64 `json:"precipitation_probability_max"`
		} `json:"daily"`
	}
	if err := c.get(ctx, "https://api.open-meteo.com/v1/forecast?"+q.Encode(), &r); err != nil {
		return nil, err
	}
	out := weatherDays{Location: p, Units: units, Days: []weatherDay{}}
	d := r.Daily
	for i, date := range d.Time {
		if i >= len(d.Code) || i >= len(d.Max) || i >= len(d.Min) {
			break
		}
		w := weatherDay{Date: date, Condition: wmoText(d.Code[i]), High: d.Max[i], Low: d.Min[i]}
		if i < len(d.Rain) && d.Rain[i] != nil {
			w.PrecipitationChance = *d.Rain[i]
		}
		out.Days = append(out.Days, w)
	}
	return out, nil
}

// wmoText names a WMO weather code, as Open-Meteo reports them.
func wmoText(code int) string {
	switch {
	case code == 0:
		return "Clear"
	case code == 1:
		return "Mainly clear"
	case code == 2:
		return "Partly cloudy"
	case code == 3:
		return "Overcast"
	case code == 45 || code == 48:
		return "Fog"
	case code >= 51 && code <= 57:
		return "Drizzle"
	case code >= 61 && code <= 67:
		return "Rain"
	case code >= 71 && code <= 77:
		return "Snow"
	case code >= 80 && code <= 82:
		return "Rain showers"
	case code == 85 || code == 86:
		return "Snow showers"
	case code >= 95:
		return "Thunderstorm"
	}
	return "Unknown"
}
