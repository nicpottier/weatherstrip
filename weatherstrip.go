package main

import (
	"bytes"
	"encoding/base64"
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

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/pkg/errors"
)

const (
	gridWidth   = 64
	gridHeight  = 16
	cellSize    = 16
	cellSpacing = 1
	snowlevel   = 1490 // 1490m, base of Tye Mill

	coldTemp = 29 // anything less than this is nice powder
	hotTemp  = 32 // anything more than this is rain

	// wsdot station
	wsdotTelemetryURL = "https://www.nwac.us/weatherdata/stevenshwy2/now/"

	// brooks station
	brooksTelemetryURL = "https://api.snowobs.com/v1/station/timeseries?token=71ad26d7aaf410e39efe91bd414d32e1db5d&stid=50&source=nwac"

	telemetryURL = brooksTelemetryURL
)

var (
	mainColor = &color.RGBA{128, 255, 255, 255}

	sunColor = &color.RGBA{168, 255, 0, 255}

	backgroundColor      = &color.RGBA{0, 0, 0, 255}
	pastSnowDayColor     = mainColor
	pastSnowNightColor   = mainColor
	futureSnowDayColor   = mainColor
	futureSnowNightColor = mainColor
	timeColor            = &color.RGBA{0, 128, 128, 255}
	flakeColor           = mainColor
	nowColor             = &color.RGBA{64, 192, 255, 255}

	coldColor = &color.RGBA{50, 168, 168, 255}
	hotColor  = &color.RGBA{139, 168, 50, 255}
)

var tempColors = map[int]*color.RGBA{
	29: &color.RGBA{50, 168, 0, 255},
	30: &color.RGBA{50, 168, 119, 255},
	31: &color.RGBA{50, 158, 58, 255},
	32: &color.RGBA{98, 168, 50, 255},
}

type HourForecast struct {
	Hour time.Time `json:"hour"`

	PredictedSnow      float64 `json:"predicted_snow,omitempty"`
	PredictedSnowLevel float64 `json:"predicted_snow_level,omitempty"`
	PredictedTemp      float64 `json:"predicted_temp,omitempty"`

	ActualSnow   float64 `json:"actual_snow,omitempty"`
	ActualTemp   float64 `json:"actual_temp,omitempty"`
	ActualPrecip float64 `json:"actual_precip,omitempty"`
}

var la *time.Location

func init() {
	la, _ = time.LoadLocation("America/Los_Angeles")
}

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

type TelemetryData struct {
	Series struct {
		Stations []struct {
			Observations struct {
				DateTime   []time.Time `json:"date_time"`
				Snow24     []float64   `json:"snow_depth_24h"`
				Snow       []float64   `json:"snow_depth"`
				Temp       []float64   `json:"air_temp"`
				HourPrecip []float64   `json:"precip_accum_one_hour"`
			} `json:"OBSERVATIONS"`
		} `json:"STATION"`
	} `json:"station_timeseries"`
}

func loadPastTelemetry(merged map[time.Time]*HourForecast, data []byte) error {
	telemetry := &TelemetryData{}
	err := json.Unmarshal(data, telemetry)
	if err != nil {
		return err
	}

	if len(telemetry.Series.Stations) == 0 {
		return errors.Errorf("no stations data")
	}

	observations := telemetry.Series.Stations[0].Observations
	for i := range observations.DateTime {
		forecast := &HourForecast{
			Hour:         observations.DateTime[i].In(la),
			ActualSnow:   observations.Snow[i],
			ActualTemp:   observations.Temp[i],
			ActualPrecip: observations.HourPrecip[i],
		}

		// subtract one hour from our forecast hour, telemetry data is taken at the top of the hour and represents
		// what happened in the previous hour
		forecast.Hour = forecast.Hour.Add(-time.Minute * 60)
		merged[forecast.Hour] = forecast
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

	now := time.Now().In(la)

	//url := fmt.Sprintf(telemetryURL, now.AddDate(0, 0, -1).Format("200601021504"), now.Format("200601021504"))
	//fmt.Println(url)

	// scrape the stevens data
	telemetryData, err := loadURLData(telemetryURL)
	if err != nil {
		log.Fatal(err)
	}

	err = loadPastTelemetry(merged, telemetryData)
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
	data, err := loadURLData("https://api.weather.gov/gridpoints/SEW/164,65")
	if err != nil {
		log.Fatal(err)
	}
	future = data

	err = loadFuture(merged, future)
	if err != nil {
		log.Fatal(err)
	}

	// print our data out
	dumpData(merged)

	// start is at 4pm the previous day
	now = time.Now().Truncate(time.Hour).In(la)
	yesterday := now.AddDate(0, 0, -1)
	start := time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(), 16, 0, 0, 0, la)

	for merged[start] == nil {
		start = start.Add(time.Hour)
	}

	// when our graph actually starts
	graphStart := now.Add(time.Hour * -4)

	// end is 64 hours in the future
	graphEnd := graphStart.Add(time.Hour * time.Duration(64))

	curr := start

	img := makeImage()

	// from start to end
	total := float64(0)
	startDepth := merged[start].ActualSnow

	for ; curr.Before(graphStart); curr = curr.Add(time.Hour) {
		forecast := merged[curr]
		if forecast == nil {
			continue
		}

		if merged[curr].ActualSnow < startDepth {
			startDepth = merged[curr].ActualSnow
		}

		// we reset accumulation at 4pm
		if curr.Hour() == 16 {
			if total > 0 {
				total = 0
			}

			startDepth = merged[curr].ActualSnow
		}

		total = forecast.ActualSnow - startDepth
	}

	fmt.Printf("start: %s graphStart: %s now: %s startDepth: %f\n", start, graphStart, now, startDepth)

	for ; curr.Before(graphEnd); curr = curr.Add(time.Hour) {
		offset := int(curr.Sub(graphStart) / time.Hour)

		//if curr.Equal(now) {
		//	setPixel(img, offset, 0, timeColor)
		//	setPixel(img, offset, 1, timeColor)
		//	setPixel(img, offset, 2, timeColor)
		//	setPixel(img, offset, 3, timeColor)
		//}

		if curr.Hour() == 0 {
			setPixel(img, offset, 14, timeColor)
			setPixel(img, offset, 15, timeColor)
		} else if curr.Hour() == 12 {
			setPixel(img, offset, 15, timeColor)
		}

		// we reset accumulation at 4pm
		if curr.Hour() == 16 {
			fmt.Printf("16 hour: %s\n", curr.String())
			if total > 0 {
				total = 0
			}

			if curr.Before(now) {
				if merged[curr] != nil {
					startDepth = merged[curr].ActualSnow
					fmt.Printf("start depth reset to: %f\n", merged[curr].ActualSnow)
				} else {
					startDepth = 0
				}
			}
		}

		forecast := merged[curr]
		if forecast == nil {
			continue
		}

		temp := float64(0)

		if curr.After(now) || curr.Equal(now) {
			temp = forecast.PredictedTemp

			if total < 0 {
				total = 0
			}

			if forecast.PredictedSnowLevel < snowlevel {
				if forecast.PredictedSnow > 0 {
					top := 1 + (offset%2)*2

					if forecast.PredictedSnow > 0 {
						setPixel(img, offset, top, flakeColor)
					}

					if forecast.PredictedSnow > .25 {
						setPixel(img, offset, top+4, flakeColor)
					}

					if forecast.PredictedSnow > .50 {
						setPixel(img, offset, top+8, flakeColor)
					}
				}

				total += forecast.PredictedSnow
			} else {
				top := 1 + (offset%2)*3
				if forecast.PredictedSnow > 0 {
					setPixel(img, offset, top, flakeColor)
					setPixel(img, offset, top+1, flakeColor)
				}
			}

			color := futureSnowDayColor
			if curr.Hour() >= 16 || curr.Hour() < 9 {
				color = futureSnowNightColor
			}

			fmt.Printf("future snow: %s\t%f\t%f\t%f\t%f\n", curr, total, forecast.PredictedSnow, forecast.PredictedSnowLevel, forecast.PredictedTemp)
			setColumn(img, offset, 16-int(total), color, false)
		} else {
			temp = forecast.ActualTemp

			if merged[curr].ActualPrecip > 0 {
				if merged[curr].ActualTemp > hotTemp {
					top := 1 + (offset%2)*3
					setPixel(img, offset, top, flakeColor)
					setPixel(img, offset, top+1, flakeColor)
				} else {
					top := 1 + (offset%2)*2
					setPixel(img, offset, top, flakeColor)
				}
			}

			if merged[curr].ActualSnow < startDepth {
				startDepth = merged[curr].ActualSnow
			}

			if forecast.ActualSnow-startDepth > total {
				total = forecast.ActualSnow - startDepth
			}
			color := pastSnowDayColor
			if curr.Hour() >= 16 || curr.Hour() < 9 {
				color = pastSnowNightColor
			}
			fmt.Printf("  past snow: %s\t%f\t%f\t%f\t%f\n", curr, total, forecast.ActualSnow, startDepth, forecast.ActualTemp)
			setColumn(img, offset, 16-int(total), color, false)
		}

		// display our temp strip
		if temp > hotTemp {
			setPixel(img, offset, 0, hotColor)
		} else if temp < coldTemp {
			setPixel(img, offset, 0, coldColor)
		} else {
			setPixel(img, offset, 0, tempColors[int(temp)])
		}

		// rewrite our time ticks in case they were written over
		if curr.Hour() == 0 {
			setPixel(img, offset, 15, timeColor)
			setPixel(img, offset, 14, timeColor)
		} else if curr.Hour() == 12 {
			setPixel(img, offset, 15, timeColor)
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
