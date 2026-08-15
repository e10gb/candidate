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
	EdgeTicks float64 // floor on the half-spread we quote, in ticks
	// SkewFrac is the inventory lean at full position, as a fraction of the
	// current edge. 1.0 means that at MaxPos the flattening side is pulled a
	// whole edge toward fair (then held off it by MinEdgeTicks), while the
	// discouraging side moves a whole edge away.
	SkewFrac float64
	// The edge is sized from measured volatility: EdgeVolMult * stdev of the mid
	// over VolWindow, floored at EdgeTicks and capped at MaxEdgeTicks. Set
	// EdgeVolMult to 0 for a fixed edge of EdgeTicks.
	EdgeVolMult float64
	// MaxEdgeFrac caps the edge at a fraction of the current price, so the cap
	// transfers to an instrument trading at a different level. MaxEdgeTicks is an
	// optional absolute backstop, off by default.
	MaxEdgeFrac  float64
	MaxEdgeTicks float64
	VolWindow    time.Duration
	// MinEdgeTicks is the margin every quote must keep against fair value, no
	// matter how much inventory skew wants to give away. Getting flat below this
	// is the hedger's job.
	MinEdgeTicks float64
	MaxTPS       int           // self-imposed sustained request rate cap
	MaxBurst     int           // requests allowed back-to-back before throttling
	MinRequote   time.Duration // floor on time between requote cycles
}

func loadConfig() config {
	return config{
		NatsURL: env("NATS_URL", "nats://127.0.0.1:4222"),
		// Compose injects these; grading injects real values.
		Sender: env("SENDER", "QUOTE001"),
		Feed:   env("QUOTER_FEED", env("TAKER_FEED", "AAH6")),

		Clip:      envInt("QUOTER_CLIP", 5),
		MaxPos:    envInt("QUOTER_MAX_POS", 25),
		EdgeTicks: envFloat("QUOTER_EDGE", 2),
		SkewFrac:  envFloat("QUOTER_SKEW_FRAC", 1.0),
		// 1 tick: the smallest margin that is still a margin.
		MinEdgeTicks: envFloat("QUOTER_MIN_EDGE", 1),
		// Sized from measured volatility rather than fitted to one market. 0 gives
		// a fixed edge of QUOTER_EDGE, which is how the fixed-edge sweep was run.
		//
		// 4.0 is a risk appetite ("quote roughly four sigma wide"), not a price
		// level, so it is the part that transfers to a market with different
		// absolute moves. A volatility-sized edge beat the original fixed edge of 2
		// by about an order of magnitude and repeatably; the choice of multiplier
		// within the wide range did not separate from run-to-run noise, so 4.0 is a
		// reasonable point in a broad plateau rather than a tuned optimum. See
		// NOTES.md for the measurements and the retraction.
		EdgeVolMult: envFloat("QUOTER_EDGE_VOL", 4.0),
		// Cap on the edge. Volatility is sampled at requote moments, and we requote
		// when the market moves, so an uncapped edge is priced off conditional
		// volatility and runs very wide -- realised quoted spread reached 240
		// against a market spread of 1-5, which is a quoter that has left the
		// market. The cap bounds that. Its exact value is not resolvable at the
		// noise level measured, so 20 is chosen to bound the pathological case, not
		// as a tuned optimum.
		//
		// MaxEdgeFrac expresses the same cap as a fraction of price, for a market
		// where risk scales with price level. It is off by default because this
		// market's moves are absolute (the sim's mover steps 20-60 points whatever
		// the price), not because it measured worse -- that comparison did not
		// survive repetition either. Set one or the other.
		MaxEdgeFrac:  envFloat("QUOTER_MAX_EDGE_FRAC", 0),
		MaxEdgeTicks: envFloat("QUOTER_MAX_EDGE", 20),
		VolWindow: time.Duration(envInt("QUOTER_VOL_WINDOW_MS", 2000)) *
			time.Millisecond,
		MaxTPS: envInt("QUOTER_MAX_TPS", 20),
		// 8 = two full two-sided repositions back-to-back, so a fast market does
		// not leave us queued behind our own rate limiter.
		MaxBurst:   envInt("QUOTER_MAX_BURST", 8),
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
	log.Printf("%s quoting %s clip=%d maxpos=%d edgefloor=%.1f skewfrac=%.1f",
		cfg.Sender, cfg.Feed, cfg.Clip, cfg.MaxPos, cfg.EdgeTicks, cfg.SkewFrac)

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
