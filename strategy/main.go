// Command quoter is the desk's market-making seat: it rests two-sided liquidity
// on one contract and earns the spread, skewing its quotes against whatever
// inventory it is carrying so it stays close to flat.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
)

type config struct {
	NatsURL string
	Sender  string
	Feed    string

	Clip      int     // size per quote
	MaxPos    int     // hard cap on absolute position
	EdgeTicks float64 // half-spread we try to earn, in ticks
	SkewTicks float64 // quote shift at full inventory, in ticks
	// MinEdgeTicks is the margin every quote must keep against fair value, no
	// matter how much inventory skew wants to give away. Getting flat below this
	// is the hedger's job.
	MinEdgeTicks float64
	MaxTPS       int           // self-imposed request rate cap
	MinRequote   time.Duration // floor on time between requote cycles
}

func loadConfig() config {
	return config{
		NatsURL: env("NATS_URL", "nats://127.0.0.1:4222"),
		// Compose injects these; grading injects real values.
		Sender: env("SENDER", "QUOTE001"),
		Feed:   env("QUOTER_FEED", env("TAKER_FEED", "AAH6")),

		Clip:       envInt("QUOTER_CLIP", 5),
		MaxPos:     envInt("QUOTER_MAX_POS", 25),
		EdgeTicks:  envFloat("QUOTER_EDGE", 2),
		SkewTicks:  envFloat("QUOTER_SKEW", 4),
		// 1 tick: the smallest margin that is still a margin.
		MinEdgeTicks: envFloat("QUOTER_MIN_EDGE", 1),
		MaxTPS:     envInt("QUOTER_MAX_TPS", 20),
		MinRequote: time.Duration(envInt("QUOTER_MIN_REQUOTE_MS", 50)) * time.Millisecond,
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return def
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.SetPrefix("[quoter] ")
	cfg := loadConfig()

	nc, err := nats.Connect(cfg.NatsURL,
		nats.Name("quoter-"+cfg.Sender),
		nats.MaxReconnects(-1), // the exchange may come up after us
		nats.ReconnectWait(time.Second),
	)
	if err != nil {
		log.Fatalf("connect %s: %v", cfg.NatsURL, err)
	}
	defer nc.Close()
	log.Printf("%s quoting %s clip=%d maxpos=%d edge=%.1f skew=%.1f",
		cfg.Sender, cfg.Feed, cfg.Clip, cfg.MaxPos, cfg.EdgeTicks, cfg.SkewTicks)

	q := newQuoter(nc, cfg)

	if _, err := nc.Subscribe("ex.bbo."+cfg.Feed, q.onBBO); err != nil {
		log.Fatalf("subscribe bbo: %v", err)
	}
	// Our own fills, straight from the exchange.
	if _, err := nc.Subscribe("ex.md."+cfg.Feed+"."+cfg.Sender, q.onOwnMD); err != nil {
		log.Fatalf("subscribe md: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go q.requoteLoop(ctx)
	go q.reportLoop(ctx)

	<-ctx.Done()
	log.Print("shutting down, pulling quotes")
	q.cancelAll()
	log.Printf("final: %s", q.status())
	_ = nc.Drain()
}
