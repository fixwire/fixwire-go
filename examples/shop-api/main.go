// Command shop-api is a small HTTP API reporting to Fixwire: each request
// gets its own scope and trace, panics are reported as crashes, a failed
// payment is reported with the order as context, a call to the inventory
// service is traced, and log records become breadcrumbs.
//
//	FIXWIRE_DSN=https://<key>@<host> go run ./shop-api
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	fixwire "github.com/fixwire/fixwire-go"
	"github.com/fixwire/fixwire-go/fixwirehttp"
)

// The catalog stands in for a database.
var products = map[string]product{
	"sku_1": {ID: "sku_1", Name: "Mug", PriceCents: 1200},
	"sku_2": {ID: "sku_2", Name: "Poster", PriceCents: 2500},
}

type product struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	PriceCents int    `json:"price_cents"`
}

// PaymentError is what the payment provider answers with.
type PaymentError struct{ Code string }

func (e *PaymentError) Error() string { return "payment declined: " + e.Code }

type app struct {
	inventory    *http.Client
	inventoryURL string
}

func main() {
	inventoryURL := env("INVENTORY_URL", "http://localhost:8081")
	// The DSN comes from FIXWIRE_DSN; without it, Fixwire does nothing.
	err := fixwire.Init(fixwire.Options{
		Release:          env("RELEASE", "shop-api@1.0.0"),
		TracesSampleRate: 1,
		// Trace headers go to our own inventory service, nowhere else.
		TracePropagationTargets: []string{inventoryURL},
	})
	if err != nil {
		log.Fatal(err)
	}
	// Log records become breadcrumbs (and events from Error up).
	slog.SetDefault(slog.New(fixwire.NewSlogHandler(slog.NewTextHandler(os.Stderr, nil), nil)))

	a := &app{
		inventory:    &http.Client{Transport: fixwirehttp.NewTransport(nil), Timeout: 5 * time.Second},
		inventoryURL: inventoryURL,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /products/{id}", a.getProduct)
	mux.HandleFunc("POST /orders", a.createOrder)
	mux.HandleFunc("GET /admin/report", a.report)

	srv := &http.Server{
		Addr:              ":" + env("PORT", "8080"),
		Handler:           fixwirehttp.New(fixwirehttp.Options{}).Handle(withUser(mux)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	slog.Info("listening", "addr", srv.Addr)

	// On SIGTERM: finish the requests under way, then send what is left.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	fixwire.Close(2 * time.Second)
}

// withUser puts the signed-in user (here: a header) on the request's scope.
func withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-User-Id"); id != "" {
			fixwire.HubFromContext(r.Context()).Scope().SetUser(fixwire.User{ID: id})
		}
		next.ServeHTTP(w, r)
	})
}

func (a *app) getProduct(w http.ResponseWriter, r *http.Request) {
	// A span for the lookup, under the request's.
	span, _ := fixwire.StartSpan(r.Context(), "SELECT products", fixwire.WithOp("db.query"),
		fixwire.WithAttributes(map[string]any{"db.system": "postgresql"}))
	p, ok := products[r.PathValue("id")]
	span.Finish()
	if !ok {
		http.Error(w, "no such product", http.StatusNotFound) // a 404 is not an error worth reporting
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *app) createOrder(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hub := fixwire.HubFromContext(ctx)
	var in struct {
		SKU  string `json:"sku"`
		Card string `json:"card"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad order", http.StatusBadRequest)
		return
	}
	hub.Scope().SetTag("sku", in.SKU)
	slog.InfoContext(ctx, "order received", "sku", in.SKU)

	if err := a.reserve(ctx, in.SKU); err != nil {
		hub.CaptureException(err)
		http.Error(w, "out of stock", http.StatusConflict)
		return
	}
	orderID := "ord_" + time.Now().Format("150405.000")
	if err := charge(in.Card); err != nil {
		// Handled: the customer gets an answer, Fixwire gets the error with the order.
		hub.WithScope(func(s *fixwire.Scope) {
			s.SetContext("order", map[string]any{"id": orderID, "sku": in.SKU})
			hub.CaptureException(fmt.Errorf("charging order %s: %w", orderID, err))
		})
		http.Error(w, "payment declined", http.StatusPaymentRequired)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": orderID})
}

// reserve asks the inventory service to hold one item: a traced call.
func (a *app) reserve(ctx context.Context, sku string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.inventoryURL+"/reservations?sku="+sku, nil)
	if err != nil {
		return err
	}
	res, err := a.inventory.Do(req)
	if err != nil {
		return fmt.Errorf("reserving %s: %w", sku, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 300 {
		return fmt.Errorf("reserving %s: inventory answered %d", sku, res.StatusCode)
	}
	return nil
}

func charge(card string) error {
	if card == "4000000000000002" { // the test card that is always declined
		return &PaymentError{Code: "card_declined"}
	}
	return nil
}

func (a *app) report(w http.ResponseWriter, r *http.Request) {
	var cents []int // today's orders: none yet
	total := 0
	for _, c := range cents {
		total += c
	}
	// A bug: with no orders this divides by zero and panics; the middleware
	// reports the panic and answers 500.
	writeJSON(w, http.StatusOK, map[string]int{"average_cents": total / len(cents)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
