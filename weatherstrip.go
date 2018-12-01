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
	hour time.Time

	predictedSnow      float64
	predictedTemp      float64
	predictedSnowLevel float64

	actualSnow float64
	actualTemp float64
}

var la *time.Location

func init() {
	la, _ = time.LoadLocation("America/Los_Angeles")
}

func loadPast(merged map[time.Time]*HourForecast, data []byte) error {
	reader := csv.NewReader(bytes.NewReader(data))
	rows, err := reader.ReadAll()
	if err != nil {
		return err
	}

	lastSnow := float64(-1)

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

		if lastSnow != -1 {
			merged[t] = &HourForecast{
				hour:       t,
				actualSnow: depth - lastSnow,
			}

		}
		lastSnow = depth
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
					hour:          valueTime,
					predictedSnow: value,
				}
			} else {
				present.predictedSnow = value
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
					hour:               valueTime,
					predictedSnowLevel: v.Value,
				}
			} else {
				present.predictedSnowLevel = v.Value
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
					hour:          valueTime,
					predictedTemp: value,
				}
			} else {
				present.predictedTemp = value
			}

			fmt.Printf("set temp to %f for %s\n", value, valueTime)
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
	img := makeImage()
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

func main() {
	// Make the handler available for Remote Procedure Call by AWS Lambda
	lambda.Start(handler)
}

func build() {
	merged := make(map[time.Time]*HourForecast)

	// read our telemetry file
	var past []byte
	if len(os.Args) == 1 {
		data, err := ioutil.ReadFile("./data/nwac_2018_11_23.csv")
		if err != nil {
			log.Fatal(err)
		}
		past = data
	} else {
		data, err := loadURLData("https://www.nwac.us/data-portal/csv/location/stevens-pass/sensortype/snow_depth/start-date/2018-11-22/end-date/2020-05-23/")
		if err != nil {
			log.Fatal(err)
		}
		past = data
	}

	err := loadPast(merged, past)
	if err != nil {
		log.Fatal(err)
	}

	// read our forecast data
	var future []byte
	if len(os.Args) == 1 {
		data, err := ioutil.ReadFile("./data/23_11_2018.json")
		if err != nil {
			log.Fatal(err)
		}
		future = data
	} else {
		data, err := loadURLData("https://api.weather.gov/gridpoints/SEW/164,65")
		if err != nil {
			log.Fatal(err)
		}
		future = data
	}

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

	now, _ := time.ParseInLocation("2006-01-02 15:04", "2018-11-23 13:00", la)
	now = now.Round(0)

	if len(os.Args) != 1 {
		now = time.Now().Round(time.Hour).In(la)
	}

	// start is 24 hours in the past
	start := now.Add(time.Hour * time.Duration(-16))

	// end is 40 hours in the future
	end := now.Add(time.Hour * time.Duration(48))

	// dump in sorted order
	for _, t := range times {
		fmt.Printf("%v - %f - %f - %f - %f\n", t, merged[t].actualSnow, merged[t].predictedSnow, merged[t].predictedSnowLevel, merged[t].predictedTemp)
	}

	img := makeImage()

	// from start to end
	total := float64(0)
	nightTotal := float64(0)

	for curr := start; curr.Before(end); curr = curr.Add(time.Hour) {
		offset := int(curr.Sub(start) / time.Hour)

		if curr == now {
			setPixel(img, offset, 0, timeColor)
			setPixel(img, offset, 1, timeColor)
			setPixel(img, offset, 2, timeColor)
		}

		if curr.Hour() == 0 {
			setPixel(img, offset, 0, timeColor)
			setPixel(img, offset, 1, timeColor)
		} else if curr.Hour() == 12 {
			setPixel(img, offset, 0, timeColor)
		}

		forecast := merged[curr]
		if forecast == nil {
			continue
		}

		if curr.After(now) {
			if forecast.predictedSnow > 0 {
				if forecast.predictedSnowLevel < snowlevel {
					if curr.Hour() < 9 || curr.Hour() > 16 {
						nightTotal += forecast.predictedSnow
					} else {
						total += forecast.predictedSnow
						total += nightTotal
						nightTotal = 0
					}
				}
				setColumn(img, offset, 16-int(total+nightTotal), futureSnowNightColor, true)
				setColumn(img, offset, 16-int(total), futureSnowDayColor, true)
			}
		} else {
			if forecast.actualSnow != 0 {
				if curr.Hour() < 9 || curr.Hour() > 16 {
					nightTotal += forecast.actualSnow
				} else {
					total += forecast.actualSnow
					total += nightTotal
					nightTotal = 0
				}
				setColumn(img, offset, 16-int(total+nightTotal), pastSnowNightColor, total > 1)
				setColumn(img, offset, 16-int(total), pastSnowDayColor, total > 1)
			}
		}
	}

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
