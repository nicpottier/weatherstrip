package main

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io/ioutil"
	"log"
	"os"
	"time"
)

const (
	gridWidth   = 64
	gridHeight  = 16
	cellSize    = 16
	cellSpacing = 1
)

func main() {
	// read our data file
	data, err := ioutil.ReadFile("./data/22_07_2018.json")
	if err != nil {
		log.Fatal(err)
	}

	forecast := Forecast{}
	err = json.Unmarshal(data, &forecast)
	if err != nil {
		log.Fatal(err)
	}

	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		log.Fatal(err)
	}

	img := makeImage()

	for i, v := range forecast.Properties.Temperature.Values {
		if i > gridWidth {
			break
		}

		// Mon Jan 2 15:04:05 MST 2006
		t, err := time.Parse("2006-01-02T15:04:05-07:00/PT1H", v.Time)
		if err != nil {
			log.Fatal(err)
		}
		t = t.In(la)

		f := toFahrenheit(v.Value)
		val := int((f - 40) / 5)

		fmt.Printf("%d - %d - %f\n", t.Hour(), val, f)

		for y := 0; y < val; y++ {
			setPixel(img, i, gridHeight-y, color.White)
		}

		if t.Hour() == 0 {
			setPixel(img, i, gridHeight-1, color.Black)
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

func toFahrenheit(c float64) float64 {
	return c*9/5 + 32
}
