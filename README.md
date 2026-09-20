# Flight Booking Microservices

A flight search and ticketing platform split into four independent services that talk over HTTP and gRPC, wired together with Docker Compose. The product side is a Persian right-to-left booking site called *Belino* (بلینو); the engineering side is a worked example of splitting authentication, business logic, and payment into separate deployables backed by two Postgres instances and Redis.

The repository ships with a generated dataset of **500,000 flights** across **1,388 airports** in **208 countries**, so the search API runs against a table large enough for indexes and query plans to matter rather than a handful of demo rows.

## The problem this solves

A booking flow has three pieces that pull in different directions:

- **Identity** needs to be fast, stateless at the edge, and revocable. A signed-out token has to stop working immediately, which a plain JWT cannot do on its own.
- **Inventory and search** needs to answer "what seats are left on flights from ADZ to BTS on 2023-02-04" against a large flight table, with seat capacity computed live from purchases.
- **Payment** is an external system you do not control, and you still have to test against it.

We split those into services so each could use the tool that fit: Go for the token path, Node for the query and booking path, Django for a payment gateway stand-in that behaves like a real redirect-and-callback flow.

## Architecture

| Component | Directory | Stack | Address |
| :-- | :-- | :-- | :-- |
| Web front end | `Front/` | React 18, MUI 5, nginx | `localhost:8000` |
| Auth core | `AuthCore/` | Go 1.18, Gin, GORM, gRPC | `authcore:5000` (HTTP), `:7132` (gRPC) |
| Ticket API | `TicketService/` | Node 16, Express 4, Sequelize, `pg` | `ticket:3000` |
| Payment gateway | `bank/` | Django 4.1, Django REST Framework | `bank:8000` |
| Ticket database | `postgres1` service | Postgres, DB `ticketservice` | `localhost:5433` |
| Auth database | `postgres/` | Postgres, DB `airport` | `localhost:5434` |
| Token cache | `redis` service | Redis with AOF persistence | `localhost:6380` |

The front end and the auth service are published to the host, on 8000 and 5000. nginx (`Front/default.conf`) is still the single entry point the browser uses, and reverse-proxies two prefixes into the private network:

```
/auth/    ->  http://authcore:5000/
/ticket/  ->  http://ticket:3000/
```

A purchase moves through the system like this:

```
browser -> nginx :8000
             |
             |-- POST /auth/signin ------> AuthCore ---> Postgres (airport)
             |                                      \--> Redis (refresh + revoked tokens)
             |
             \-- POST /ticket/ticket ----> TicketService
                                              |
                                              |-- POST http://bank:8000/transaction/
                                              |      returns {id, ...}
                                              |
                                              \-- responds with redirect_url
                                                     http://bank:8000/payment/<id>

browser -> bank /payment/<id>        (pick an outcome on the mock gateway page)
bank    -> GET /payed/<id>/<result>/ (stores the result, then 302 to the callback)
        -> TicketService /transactionResult/<id>/<result>
```

The two databases are deliberately separate. `AuthCore` owns `user_account`, `refresh_token` and `unauthorized_token` in the `airport` database; `TicketService` owns `flight`, `purchase` and the reference tables in `ticketservice`. Neither service reads the other's tables. When the ticket side needs to know who the caller is, it asks over gRPC (`CheckToken`) instead of sharing a schema.

## Auth core (Go)

`AuthCore/api/api.go` implements the whole token lifecycle in about 420 lines.

- **Sign up** validates gender, an 11 digit phone number, an email pattern and a minimum password length, then stores a SHA-256 hash of the password.
- **Sign in** issues two HS256 JWTs from one shared key: an access token with a 30 minute max age and a refresh token with a 30 day max age. The claim set carries `is_refresh`, `user_id`, `email` and `phone_number`, so the token type is part of the signed payload and a refresh token cannot be replayed as an access token.
- **Sign out** is the interesting part. Instead of waiting for expiry, the access token is written into an `unauthorized_token` row *and* into Redis with a TTL equal to the token's remaining life. Every subsequent check hits Redis first and only falls back to Postgres on a cache miss, and the same sign-out transaction deletes rows where `expiration < NOW()` so the revocation table stays bounded.
- **Refresh** verifies the signature, rejects anything without `is_refresh`, checks Redis, falls back to the `refresh_token` table, deletes the row if it has expired, and mints a new access token.
- **gRPC** exposes one method, `AuthService.CheckToken(Token) returns (UserData)` (`AuthCore/proto/auth.proto`), which reuses the same `userInfo` path as the HTTP endpoint. `TicketService/middlewares/protos/auth.proto` holds a matching copy of the contract.

HTTP endpoints, all `POST`:

| Endpoint | Auth | Purpose |
| :-- | :-- | :-- |
| `/signup` | none | create an account, returns the user record |
| `/signin` | none | returns `access_token`, `refresh_token`, `expiration` |
| `/refresh` | refresh token in `Authorization` | returns a new `access_token` |
| `/userinfo` | access token in `Authorization` | returns the user record |
| `/signout` | access token in `Authorization` | revokes the access token |

## Ticket API (Node)

`TicketService/` is an Express app with a router, middleware, controller, service split.

| Endpoint | Method | Notes |
| :-- | :-- | :-- |
| `/flights` | GET | `origin`, `destination`, `departureDate` required; `returnDate` optional |
| `/ticket` | POST | creates a bank transaction and returns `redirect_url` |
| `/transactionResult/:transactionId/:resultId` | GET | callback target for the payment gateway |
| `/dashboard/tickets` | GET | purchases belonging to the caller |

Two details worth calling out:

**Validation lives in middleware, not controllers.** `middlewares/flightMiddleware.js` uses `express-validator` to require the three search parameters, enforce `YYYY-MM-DD`, and run a custom check that the return date is not before the departure date. It also sets `req.hasReturn` as a side effect so the controller can branch on round trip versus one way without re-reading the query string.

**Search is a raw parameterised query against a database view, not an ORM traversal.** `services/flightService.js` runs:

```sql
SELECT * FROM available_offers
WHERE origin = $1 AND destination = $2
  AND $3 = DATE_TRUNC('day', departure_local_time)
```

`available_offers` (defined in `postgresdata/ticket/init.sql`) is where the real work happens. It joins `flight` to the `aircraft_view` for seat counts, converts `departure_utc` into the origin airport's local time and the arrival into the destination's local time using each city's IANA timezone name, and subtracts confirmed purchases per cabin to produce `y_class_free_capacity`, `j_class_free_capacity` and `f_class_free_capacity`. The API layer never computes availability; the view does.

Sequelize is used only for the write path (`models/purchase.js`, `models/flightModel.js`) while reads go through a `pg` connection pool (`services/database.js`).

## Payment gateway (Django)

`bank/` is a deliberately small stand-in for a payment processor, kept so the booking flow can be exercised end to end without a real provider. It exposes a DRF `ModelViewSet` at `/transaction/` for creating a transaction with an `amount`, a `receipt_id` and a `callback` URL, a page at `/payment/<id>/` that renders the transaction and offers five outcomes, and `/payed/<id>/<result>/` which records the outcome and redirects to `callback + /<id>/<result>`.

The result codes the ticket service understands (`routes/transactionResult.js`):

| Code | Meaning |
| :-- | :-- |
| 1 | success |
| 2 | input mismatch |
| 3 | expired |
| 4 | no credit |
| 5 | cancelled |

Being able to pick the failure mode by clicking a button is the whole point: every branch of the callback handler is reachable without waiting for a real card to decline.

## Front end (React)

`Front/` is a Create React App build served by nginx from a two stage Dockerfile (`node:16-alpine` builds, `nginx:1.23-alpine` serves).

- Right-to-left layout end to end: `stylis-plugin-rtl` in an Emotion cache, `direction: 'rtl'` on the MUI theme, and `document.body.dir = "rtl"` set in a layout effect (`src/App.js`).
- Persian typography via the bundled Vazir font family, and `moment-jalali` plus `react-persian-datepicker` for Jalali dates.
- Form state with Formik, validation with Yup schemas that emit Persian error messages (`src/validationSchema/authSchema.js` enforces an Iranian `09XXXXXXXXX` phone pattern, an 8 character minimum password, and a confirm-password match).
- Server state with TanStack Query: `useGetAvailableTickets` keys the flight search on `[origin, destination, departureDate, returnDate]`, so going back to a previous search is served from cache.
- Prices are rendered in Persian-Indic digits (`toPersianNumber` in `src/components/TicketView.jsx`), and a cabin's buy button is disabled from the free-capacity fields the `available_offers` view returns.

## The seeded dataset

`postgresdata/ticket/` holds the schema and the data used to fill it. This is generated reference and synthetic data, not a Postgres data directory.

| File | Rows | What it is |
| :-- | --: | :-- |
| `csvs/country.csv` | 208 | country names |
| `csvs/city.csv` | 1,341 | city plus IANA timezone name |
| `csvs/airport.csv` | 1,388 | real airport names and IATA codes |
| `csvs/aircraft_type.csv` | 33 | manufacturer, model, series |
| `csvs/aircraft_layout.csv` | 101 | seat counts per cabin for a type |
| `csvs/aircraft.csv` | 99 | registrations mapped to a layout |
| `csvs/flight.csv` | 500,000 | synthetic flights with route, aircraft, departure, duration and three cabin prices |
| `init.sql` | | tables, indexes and the four views |

The schema is not flat. `aircraft_view` flattens type and layout into one row per registration; `airport_timezone` pairs an IATA code with its city timezone; `origin_destination` unions individual airports with a synthetic `ALL` entry per city so a search can target "all airports in this city"; `available_offers` is the view the search API reads. Indexes exist on `flight (flight_id)` and on `flight (origin, destination, departure_utc)`, which is the composite the search filters on.

`flight.csv` is roughly 37 MB and accounts for about 91% of the repository size.

## Running it

```bash
git clone https://github.com/Imanm02/Flight-Booking-Microservices.git
cd Flight-Booking-Microservices
docker compose up --build
```

Then open `http://localhost:8000`.

On first start, `postgresdata/ticket/init.sql` runs automatically in the ticket database and creates the schema, and `postgresdata/ticket/csvs` is mounted inside that container at `/fakedata`. The CSV files are **not** imported automatically, so the tables are empty until you load them. Load in foreign key order:

```bash
docker compose exec postgres1 psql -U postgres -d ticketservice \
  -c "\copy country        FROM '/fakedata/country.csv'        CSV HEADER" \
  -c "\copy city           FROM '/fakedata/city.csv'           CSV HEADER" \
  -c "\copy airport        FROM '/fakedata/airport.csv'        CSV HEADER" \
  -c "\copy aircraft_type  FROM '/fakedata/aircraft_type.csv'  CSV HEADER" \
  -c "\copy aircraft_layout FROM '/fakedata/aircraft_layout.csv' CSV HEADER" \
  -c "\copy aircraft       FROM '/fakedata/aircraft.csv'       CSV HEADER" \
  -c "\copy flight         FROM '/fakedata/flight.csv'         CSV HEADER"
```

`flight_serial` and `layout_id` are `SERIAL` columns and the CSVs supply them explicitly, so the sequences do not advance during the copy. Fix them up afterwards if you plan to insert new rows:

```bash
docker compose exec postgres1 psql -U postgres -d ticketservice \
  -c "SELECT setval('flight_flight_serial_seq', (SELECT MAX(flight_serial) FROM flight))" \
  -c "SELECT setval('aircraft_layout_layout_id_seq', (SELECT MAX(layout_id) FROM aircraft_layout))"
```

A sanity check once the data is in:

```bash
curl "http://localhost:8000/ticket/flights?origin=ADZ&destination=BTS&departureDate=2023-02-04"
```

Both databases are also published to the host (`5433` for `ticketservice`, `5434` for `airport`) if you want to inspect them directly.

### Running one service on its own

`AuthCore/docker-compose.yml` brings up just Postgres, Redis and pgAdmin for working on the Go service in isolation. The Django gateway runs standalone with `pip install django djangorestframework && python manage.py migrate && python manage.py runserver`, and the React app with `yarn install && yarn start` from `Front/`.

## Load testing

`locust/` holds two Locust files and the charts from the runs we recorded.

`locust/locustAuthService/locustfile.py` drives the full token lifecycle in a single task, which is the useful shape for an auth service: sign in, refresh, read user info, assert the returned email matches, sign out. Each iteration therefore touches Postgres, Redis and both JWT paths.

From `locust/locustAuthService/assets/total_requests_per_second_auth.png`, a run at 10 concurrent users:

| Metric | Value |
| :-- | :-- |
| Concurrent users | 10 |
| Sustained throughput | about 90 requests/second |
| Median response time | about 95 ms |
| 95th percentile | about 200 ms |
| Failures | none recorded for the run |

`locust/locustTicketService/locustfile.py` weights flight search 3:1 against dashboard reads and purchases, since search is the hot path. `locust/locustTicketService/assets/multiUser/` records a run at 300 users with `wait_time = between(1, 4)`: a median around 9 ms and a 95th percentile around 25 to 30 ms on the search path, no failures, at about 9.5 requests per second. The `singleUser/` charts are from a one-user run and sit near zero throughout.

One caveat worth stating rather than hiding: the ticket scenario sends a placeholder `Authorization` header and the routes run under `dummyIsAuth`, so these numbers measure search and purchase with the token check bypassed. They are useful for comparing search throughput against purchase throughput, not as an end-to-end figure for a secured deployment.

Run them yourself with:

```bash
pip install locust
locust -f locust/locustAuthService/locustfile.py --host http://localhost:5000
locust -f locust/locustTicketService/locustfile.py --host http://localhost:8000/ticket
```

## Repository layout

```
AuthCore/          Go auth service
  api/             HTTP handlers, gRPC server, request and response types
  proto/           auth.proto and generated Go stubs
  storage/         GORM models for user_account, refresh_token, unauthorized_token
TicketService/     Express ticket and booking API
  routes/          flights, ticket, transactionResult, userDashboard
  middlewares/     express-validator chains, gRPC auth client, proto copy
  controllers/     request handling
  services/        pg pool, flight search, user ticket lookup
  models/          Sequelize models
bank/              Django payment gateway stand-in
Front/             React 18 front end, nginx config, two stage Dockerfile
postgres/          Dockerfile and init.sql for the auth database
postgresdata/      schema, views and seed CSVs for the ticket database
locust/            load test scripts and recorded charts
docker-compose.yml one command bring-up of all seven containers
```

## Known gaps

I would rather list these than let someone discover them by running the stack:

- **The gRPC server never starts.** In `AuthCore/main.go`, `r.Run(":5000")` blocks, so the `s.Serve(listener)` call on `:7132` below it is unreachable. The proto contract and the `CheckToken` handler are both complete; only the startup ordering is wrong.
- **The ticket service uses a stub identity.** `TicketService/middlewares/auth.js` defines a real gRPC `isAuth` middleware, but the client construction is commented out, so routes are mounted with `dummyIsAuth`, which injects a fixed user. This is the other half of the same unfinished wiring.
- **`AuthCore` ignores its environment variables.** `docker-compose.yml` sets `REDIS_URL`, `DATABASE_URL`, `HTTP_LISTEN` and `GRPC_LISTEN_ADDRESS`, but `main.go` hardcodes `redis:6379`, the Postgres DSN, port `5000` and port `7132`. The compose `DATABASE_URL` even points at a host named `mydb` that no service defines; it works only because the code ignores it.
- **The `purchase` table is missing two columns.** `init.sql` creates `purchase` without `transaction_id` or `transaction_result`, but `models/purchase.js` declares both and `services/userTicketService.js` filters on `transaction_result = 1`. `Purchase.sync()` will not add columns to an existing table, so the insert in `routes/transaction.js` fails and is swallowed by a bare `catch`.
- **The Django gateway binds to localhost.** `bank/Dockerfile` runs `manage.py runserver` with no address, which listens on `127.0.0.1:8000` inside the container and is therefore not reachable at `bank:8000` from the ticket service. It needs `runserver 0.0.0.0:8000`.
- **The front end's auth calls do not match the auth API.** `Front/src/api/users.js` calls `users`, `tokens`, and `users/:id`, while `AuthCore` serves `/signup`, `/signin`, `/userinfo`, `/refresh` and `/signout`. The search and purchase paths do line up.
- **Passwords are SHA-256 without a salt.** `AuthCore/api/api.go` hashes with a single SHA-256 pass. A password hash should use bcrypt, scrypt or Argon2 with a per-user salt.
- **Secrets are in the source tree.** The JWT signing key is a constant in `AuthCore/api/api.go`, the Postgres DSN is a literal in `main.go`, database passwords sit in `docker-compose.yml`, and `wp_bank/settings.py` carries the generated Django development key with `DEBUG = True`. Everything here is local-only and should be replaced with environment configuration before any deployment. `TicketService/bin/www` also prints `PG_USER` and `PG_PASSWORD` to stdout on boot.

## Credits

Built with [Arash Yadegari](https://github.com/Arash1381-y) and [Hasti Karimi](https://github.com/HastiKarimi).

The `bank/` service began as a shared mock gateway published by the Sharif Web Tech Team and was adapted here. The bundled [Vazir font](https://github.com/rastikerdar/vazir-font) by Saber Rastikerdar keeps its own licence in `Front/src/assets/fonts/LICENSE`. Everything else is MIT licensed, see `LICENSE`.

---

### Background

This started as a term project for a web programming course at Sharif University of Technology in 2022, which is why the stack is deliberately varied: the brief was to build something that actually needed more than one language and more than one datastore. The code has been kept as it was written, gaps included.
