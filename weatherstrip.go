package main

import (
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

const (
	gridWidth   = 64
	gridHeight  = 16
	cellSize    = 16
	cellSpacing = 1
	snowlevel   = 1363 // 1490ft, base of Tye Mill
)

var (
	backgroundColor      = &color.RGBA{0, 0, 0, 255}
	pastSnowDayColor     = &color.RGBA{192, 192, 192, 255}
	pastSnowNightColor   = &color.RGBA{255, 255, 255, 255}
	futureSnowDayColor   = &color.RGBA{192, 192, 192, 255}
	futureSnowNightColor = &color.RGBA{255, 255, 255, 255}
	timeColor            = &color.RGBA{64, 64, 64, 255}
)

type HourForecast struct {
	Hour time.Time `json:"hour"`

	PredictedSnow      float64 `json:"predicted_snow,omitempty"`
	PredictedSnowLevel float64 `json:"predicted_snow_level,omitempty"`
	PredictedTemp      float64 `json:"predicted_temp,omitempty"`

	ActualSnow float64 `json:"actual_snow,omitempty"`
	ActualTemp float64 `json:"actual_temp,omitempty"`
}

var la *time.Location

func init() {
	la, _ = time.LoadLocation("America/Los_Angeles")
}

// TD column indexes
const (
	DateIDX   = 0
	HourIDX   = 1
	TempIDX   = 2
	RainHour  = 4
	RainTotal = 5
	Snow24    = 6
	SnowTotal = 7
)

func dumpData(merged map[time.Time]*HourForecast) {
	// get all our times
	times := make([]time.Time, 0, len(merged))
	for t := range merged {
		times = append(times, t)
	}

	// sort them
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })

	// build a sorted list of our forecasts
	forecasts := make([]*HourForecast, len(merged))

	// dump in sorted order
	for i, t := range times {
		forecasts[i] = merged[t]
	}

	dumped, _ := json.MarshalIndent(forecasts, "", "  ")
	fmt.Println(string(dumped))
}

func loadPastHTML(merged map[time.Time]*HourForecast, data []byte) error {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(data))
	if err != nil {
		log.Fatal(err)
	}
	now := time.Now()

	doc.Find("table tr").Each(func(tr int, s *goquery.Selection) {
		// we start at the 4th tr
		if tr >= 3 {
			date := ""
			values := make([]int, 12)

			s.Find("td").Each(func(td int, s *goquery.Selection) {
				if td == DateIDX {
					date = s.Text()
				}

				// otherwise parse as a date
				val, _ := strconv.Atoi(strings.TrimSpace(s.Text()))
				values[td] = val
			})

			parts := strings.Split(date, "/")
			if len(parts) != 2 {
				return
			}
			month, _ := strconv.Atoi(parts[0])
			day, _ := strconv.Atoi(parts[1])

			// account for early jan but report in dec
			year := now.Year()
			if now.Month() == 1 && month == 12 {
				year = year - 1
			}

			forecast := &HourForecast{
				Hour:       time.Date(year, time.Month(month), day, values[HourIDX]/100, 0, 0, 0, la),
				ActualSnow: float64(values[SnowTotal]),
				ActualTemp: float64(values[TempIDX]),
			}
			merged[forecast.Hour] = forecast
		}
	})

	return nil
}

func loadPast(merged map[time.Time]*HourForecast, data []byte) error {
	reader := csv.NewReader(bytes.NewReader(data))
	rows, err := reader.ReadAll()
	if err != nil {
		return err
	}

	for i := len(rows) - 1; i >= 1; i-- {
		// first row is our time, looks like: 2018-11-23 13:00
		t, err := time.ParseInLocation("2006-01-02 15:04", rows[i][0], la)
		if err != nil {
			return err
		}
		t = t.Round(0)

		// last row is brooks total snow depth
		depth := float64(0)
		if rows[i][4] != "" {
			val, err := strconv.ParseFloat(rows[i][4], 10)
			if err != nil {
				log.Printf("error parsing depth: %s, ignoring: %s", rows[i][4], err)
			} else {
				depth = val
			}
		}

		merged[t] = &HourForecast{
			Hour:       t,
			ActualSnow: depth,
		}
	}

	return nil
}

func loadFuture(merged map[time.Time]*HourForecast, data []byte) error {
	forecast := Forecast{}
	err := json.Unmarshal(data, &forecast)
	if err != nil {
		return err
	}

	regex := regexp.MustCompile("PT(\\d+)H")

	for _, v := range forecast.Properties.SnowFallAmount.Values {
		// split on /
		parts := strings.Split(v.Time, "/")

		// Mon Jan 2 15:04:05 MST 2006
		t, err := time.ParseInLocation("2006-01-02T15:04:05+00:00", parts[0], la)
		if err != nil {
			return err
		}
		t = t.Round(0)
		in := toInch(v.Value)

		// figure out range this represents
		hourMatch := regex.FindAllStringSubmatch(parts[1], 1)
		if len(hourMatch) == 0 {
			log.Printf("unable to find range for: %s\n", parts[1])
			continue
		}

		hours, err := strconv.Atoi(hourMatch[0][1])
		if err != nil {
			return err
		}

		for h := 0; h < hours; h++ {
			valueTime := t.Add(time.Hour * time.Duration(h))
			value := in / float64(hours)

			present := merged[valueTime]
			if present == nil {
				merged[valueTime] = &HourForecast{
					Hour:          valueTime,
					PredictedSnow: value,
				}
			} else {
				present.PredictedSnow = value
			}
		}
	}

	for _, v := range forecast.Properties.SnowLevel.Values {
		// split on /
		parts := strings.Split(v.Time, "/")

		// Mon Jan 2 15:04:05 MST 2006
		t, err := time.ParseInLocation("2006-01-02T15:04:05+00:00", parts[0], la)
		if err != nil {
			return err
		}
		t = t.Round(0)

		// figure out range this represents
		hourMatch := regex.FindAllStringSubmatch(parts[1], 1)
		if len(hourMatch) == 0 {
			log.Printf("unable to find range for: %s\n", parts[1])
			continue
		}

		hours, err := strconv.Atoi(hourMatch[0][1])
		if err != nil {
			return err
		}

		for h := 0; h < hours; h++ {
			valueTime := t.Add(time.Hour * time.Duration(h))

			present := merged[valueTime]
			if present == nil {
				merged[valueTime] = &HourForecast{
					Hour:               valueTime,
					PredictedSnowLevel: v.Value,
				}
			} else {
				present.PredictedSnowLevel = v.Value
			}
		}
	}

	for _, v := range forecast.Properties.Temperature.Values {
		// split on /
		parts := strings.Split(v.Time, "/")

		// Mon Jan 2 15:04:05 MST 2006
		t, err := time.ParseInLocation("2006-01-02T15:04:05+00:00", parts[0], la)
		if err != nil {
			return err
		}
		t = t.Round(0)

		// figure out range this represents
		hourMatch := regex.FindAllStringSubmatch(parts[1], 1)
		if len(hourMatch) == 0 {
			log.Printf("unable to find range for: %s\n", parts[1])
			continue
		}

		hours, err := strconv.Atoi(hourMatch[0][1])
		if err != nil {
			return err
		}

		value := toFahrenheit(v.Value)

		for h := 0; h < hours; h++ {
			valueTime := t.Add(time.Hour * time.Duration(h))

			present := merged[valueTime]
			if present == nil {
				merged[valueTime] = &HourForecast{
					Hour:          valueTime,
					PredictedTemp: value,
				}
			} else {
				present.PredictedTemp = value
			}
		}
	}

	return nil
}

func loadURLData(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return ioutil.ReadAll(resp.Body)
}

func handler(request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	img := buildImage()
	buff := &bytes.Buffer{}
	if err := png.Encode(buff, img); err != nil {
		log.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(buff.Bytes())

	return events.APIGatewayProxyResponse{
		StatusCode:      200,
		Body:            encoded,
		Headers:         map[string]string{"Content-Type": "image/png"},
		IsBase64Encoded: true,
	}, nil
}

func buildImage() *image.RGBA {
	merged := make(map[time.Time]*HourForecast)

	// scrape the stevens data
	var html []byte
	data, err := loadURLData("https://www.nwac.us/weatherdata/stevenshwy2/now/")
	if err != nil {
		log.Fatal(err)
	}
	html = data

	err = loadPastHTML(merged, html)
	if err != nil {
		log.Fatal(err)
	}

	// read our telemetry file
	//var past []byte
	//data, err = loadURLData("https://www.nwac.us/data-portal/csv/location/stevens-pass/sensortype/snow_depth/start-date/2018-11-22/end-date/2020-05-23/")
	//if err != nil {
	// log.Fatal(err)
	//}
	//past = data

	//err = loadPast(merged, past)
	//if err != nil {
	//	log.Fatal(err)
	//}

	// read our forecast data
	var future []byte
	data, err = loadURLData("https://api.weather.gov/gridpoints/SEW/164,65")
	if err != nil {
		log.Fatal(err)
	}
	future = data

	err = loadFuture(merged, future)
	if err != nil {
		log.Fatal(err)
	}

	// get all our times
	times := make([]time.Time, 0, len(merged))
	for t := range merged {
		times = append(times, t)
	}

	// sort them
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })

	// start is at 4pm the previous day
	now := time.Now().Round(time.Hour).In(la)
	yesterday := now.AddDate(0, 0, -1)
	start := time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(), 16, 0, 0, 0, la)

	// end is 64 hours in the future
	end := start.Add(time.Hour * time.Duration(64))

	// print our data out
	dumpData(merged)

	img := makeImage()

	// from start to end
	total := float64(0)

	startDepth := merged[start].ActualSnow

	for curr := start; curr.Before(end); curr = curr.Add(time.Hour) {
		offset := int(curr.Sub(start) / time.Hour)

		if curr == now {
			setColumn(img, offset, 0, timeColor, false)
		}

		if curr.Hour() == 0 {
			setPixel(img, offset, 0, timeColor)
			setPixel(img, offset, 1, timeColor)
		} else if curr.Hour() == 12 {
			setPixel(img, offset, 0, timeColor)
		}

		// we reset accumulation at 4pm
		if curr.Hour() == 16 {
			total = 0

			if curr.Before(now) {
				startDepth = merged[curr].ActualSnow
			}
		}

		forecast := merged[curr]
		if forecast == nil {
			continue
		}

		if curr.After(now) {
			if forecast.PredictedSnowLevel < snowlevel {
				total += forecast.PredictedSnow
			}
			color := futureSnowDayColor
			if curr.Hour() >= 16 || curr.Hour() < 9 {
				color = futureSnowNightColor
			}

			setColumn(img, offset, 16-int(total), color, false)
		} else {
			total = forecast.ActualSnow - startDepth
			color := pastSnowDayColor
			if curr.Hour() >= 16 || curr.Hour() < 9 {
				color = pastSnowNightColor
			}
			setColumn(img, offset, 16-int(total), color, false)
		}
	}

	return img
}

type Forecast struct {
	Properties struct {
		Temperature struct {
			Values []struct {
				Time  string  `json:"validTime"`
				Value float64 `json:"value"`
			} `json:"values"`
		} `json:"temperature"`
		SnowFallAmount struct {
			Values []struct {
				Time  string  `json:"validTime"`
				Value float64 `json:"value"`
			} `json:"values"`
		} `json:"snowFallAmount"`
		SnowLevel struct {
			Values []struct {
				Time  string  `json:"validTime"`
				Value float64 `json:"value"`
			} `json:"values"`
		} `json:"snowLevel"`
	} `json:"properties"`
}

func makeImage() *image.RGBA {
	img := image.NewRGBA(
		image.Rect(
			0,
			gridHeight*cellSize+((gridHeight+1)*cellSpacing),
			gridWidth*cellSize+((gridWidth+1)*cellSpacing),
			0,
		),
	)

	for x := 0; x < img.Bounds().Max.X; x++ {
		for y := 0; y < img.Bounds().Max.Y; y++ {
			img.Set(x, y, color.Black)
		}
	}

	return img
}

func setPixel(img *image.RGBA, x int, y int, c color.Color) {
	x = 1 + x*cellSize + x*cellSpacing
	y = 1 + y*cellSize + y*cellSpacing

	for i := 0; i < cellSize; i++ {
		for j := 0; j < cellSize; j++ {
			img.Set(x+i, y+j, c)
		}
	}
}

func setColumn(img *image.RGBA, x int, y int, c color.Color, snowing bool) {
	for yy := y; yy < gridHeight; yy++ {
		setPixel(img, x, yy, c)
	}

	if snowing && y == 16 {
		setPixel(img, x, 15, c)
	}
}

func toFahrenheit(c float64) float64 {
	return c*9/5 + 32
}

func toInch(mm float64) float64 {
	return mm / 25.4
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "test" {
		img := buildImage()
		f, err := os.Create("weatherstrip.png")
		if err != nil {
			log.Fatal(err)
		}
		if err := png.Encode(f, img); err != nil {
			f.Close()
			log.Fatal(err)
		}
		if err := f.Close(); err != nil {
			log.Fatal(err)
		}
	} else {
		// start our AWS Handler
		lambda.Start(handler)
	}
}
