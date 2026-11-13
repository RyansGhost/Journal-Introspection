// sample writes a made-up journal into a directory, so the timeline has
// something to show before it is pointed at real files.
//
//	go run ./cmd/sample DIR [DAYS]
package main

import (
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	good   = strings.Fields("great love enjoy grateful happy fun good wonderful proud peace calm rested")
	bad    = strings.Fields("tired anxious worried stressed sad lonely overwhelmed deadline sick")
	topics = strings.Fields("work faith projects family health friends")
	extras = []string{"Slept well.", "Went for a long walk for exercise.", "", "Had an argument at lunch, some conflict."}
	times  = []string{"06:40", "07:37", "12:15", "21:10", "22:45"}
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: sample DIR [DAYS]")
	}
	days := 400
	if len(os.Args) > 2 {
		days, _ = strconv.Atoi(os.Args[2])
	}
	dir := filepath.Join(os.Args[1], "journal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatal(err)
	}
	rng := rand.New(rand.NewSource(4)) // fixed seed: the same journal every time
	today := time.Now()
	for i := days; i >= 0; i-- {
		if rng.Float64() > 0.65 {
			continue // not every day gets an entry
		}
		date := today.AddDate(0, 0, -i)
		// Mood drifts in slow waves so the chart has a shape.
		wave := math.Sin(float64(i)/45) + 0.5*math.Sin(float64(i)/9)
		picked := rng.Perm(len(times))[:1+rng.Intn(2)]
		sort.Ints(picked)
		var b strings.Builder
		fmt.Fprintf(&b, "* %s\n\n", date.Format("Monday, 2006-01-02"))
		for _, t := range picked {
			var feelings []string
			for range 2 + rng.Intn(4) {
				pool := bad
				if rng.Float64() < 0.5+0.3*wave {
					pool = good
				}
				feelings = append(feelings, pool[rng.Intn(len(pool))])
			}
			fmt.Fprintf(&b, "** %s\n\nWrote about %s today. Feeling %s. %s\n\n", times[t],
				topics[rng.Intn(len(topics))], strings.Join(feelings, ", "), extras[rng.Intn(len(extras))])
		}
		if err := os.WriteFile(filepath.Join(dir, date.Format("20060102")+".org"), []byte(b.String()), 0o644); err != nil {
			log.Fatal(err)
		}
	}
}
