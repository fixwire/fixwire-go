# Shop API (net/http)

A small JSON API with Fixwire set up the way a production service would be.

```sh
FIXWIRE_DSN=https://<key>@<host> go run ./shop-api
```

It listens on `:8080` (`PORT`) and reserves stock at an inventory service
(`INVENTORY_URL`, default `http://localhost:8081`; without one, orders fail
at the reservation, and that is reported too). Then:

```sh
curl localhost:8080/products/sku_1                      # 200, with a database span
curl localhost:8080/products/nope                       # 404: not reported
curl -H 'X-User-Id: user-1' -d '{"sku":"sku_1","card":"4242424242424242"}' localhost:8080/orders
curl -H 'X-User-Id: user-2' -d '{"sku":"sku_1","card":"4000000000000002"}' localhost:8080/orders
curl localhost:8080/admin/report                        # a panic: reported, answered 500
```

What arrives in Fixwire:

- **The declined payment** as an error of `POST /orders`: the chain
  (`charging order …` caused by `main.PaymentError`), the user `user-2`,
  the `sku` tag, the order as context, and the breadcrumbs that led to it
  (the `order received` log line, the call to the inventory service).
- **The panic** in `GET /admin/report` as a crash (`integer divide by
  zero`), with the stack where it happened.
- **A trace per request**, named after its route (`GET /products/{id}`),
  with the database lookup and the call to the inventory service under it.
  The inventory service gets a `traceparent` header and continues the
  trace; other hosts get none (`TracePropagationTargets`).
- **Release health** for `shop-api@1.0.0`: each request is a session, ended
  well, with an error, or crashed.

How it is wired, in `main.go`:

```go
fixwire.Init(fixwire.Options{
	Release:                 "shop-api@1.0.0",
	TracesSampleRate:        1,
	TracePropagationTargets: []string{inventoryURL},
})
slog.SetDefault(slog.New(fixwire.NewSlogHandler(slog.NewTextHandler(os.Stderr, nil), nil)))
inventory := &http.Client{Transport: fixwirehttp.NewTransport(nil)}
handler := fixwirehttp.New(fixwirehttp.Options{}).Handle(withUser(mux))
// … and when stopped (SIGTERM, Ctrl-C or Ctrl-Break): srv.Shutdown(ctx), then fixwire.Close(2 * time.Second)
```

Handlers reach their request's scope with
`fixwire.HubFromContext(r.Context())`.
