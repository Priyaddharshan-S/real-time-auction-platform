# Live Auctions

Real-time bidding app in Go. One service serves the UI, the API and the WebSocket on one URL.
Postgres (Supabase) is the source of truth, Supabase Auth handles logins, Redis (Upstash) handles live updates, rate limiting and locks.

## What you need (all free)
A GitHub account, a [Supabase](https://supabase.com) account, an [Upstash](https://upstash.com) account and a [Render](https://render.com) account.

## Step 1: Supabase (database + login)
1. **New project**. Pick a region near your users. Set a database password and **save it**.
2. Click the **Connect** button (top of the project) and choose **Transaction pooler** (port **6543**). Copy the URI and replace `[YOUR-PASSWORD]` with your password. If the password has special characters (`@ # / :`) URL-encode them (`@` becomes `%40`). This is your `DATABASE_URL`.
3. **Project Settings → API**: copy the **Project URL** (`SUPABASE_URL`) and the **anon public** key (`SUPABASE_ANON_KEY`, on the *Legacy API keys* tab if shown). Never use the `service_role` key.
4. **Authentication → Sign In / Providers → Email**: keep **Confirm email** ON for production.
5. **Authentication → Emails → SMTP Settings**: enable **custom SMTP** (Resend or Brevo have free tiers). Supabase's built-in sender allows only a few emails per hour and causes "email rate limit exceeded".

## Step 2: Upstash (Redis)
1. **Create Database** → type **Redis**, pick a region near Render, keep **TLS** on.
2. Open the database and copy the **TCP / Redis URL** (starts with `rediss://default:...@...upstash.io:6379`). Not the REST URL. This is your `REDIS_URL`.

## Step 3: Push to GitHub
1. On github.com click **New repository** (private is fine), do **not** add a README.
2. In VS Code open the project folder, then in the terminal:
```bash
git init
git add .
git commit -m "Live auctions"
git branch -M main
git remote add origin https://github.com/YOUR_NAME/YOUR_REPO.git
git push -u origin main
```
`.env` is git-ignored. Make sure `go.mod` and `go.sum` were committed (`git ls-files | grep go.sum`).

## Step 4: Deploy on Render
1. Render dashboard → **New + → Blueprint** → connect GitHub → pick the repo. Render reads `render.yaml`.
2. Fill the five secrets when asked: `DATABASE_URL`, `REDIS_URL`, `SUPABASE_URL`, `SUPABASE_ANON_KEY`, `ADMIN_EMAIL` (the email you will sign up with). `PORT` is already set.
3. Click **Apply**. The first build takes a few minutes. Wait for **Live**, then open the `https://….onrender.com` URL.
4. On first boot the app creates all tables itself and adds 3 demo auctions. No terminal needed.
5. **Free tier note:** the service sleeps after ~15 minutes idle; the next visit takes about 50 seconds. The site shows "Connecting…" while it wakes.

## Step 5: Point Supabase at your site
Supabase → **Authentication → URL Configuration**: set **Site URL** to your Render URL and add it under **Redirect URLs**. Confirmation emails then open your real site.

## Step 6: First admin login
1. Open your Render URL → **Log in / Sign up** → sign up with exactly the `ADMIN_EMAIL` address.
2. Click the link in the confirmation email (the admin role is only granted to a verified email).
3. Log in. An **Admin** button appears in the top bar → open the admin panel.
4. Promote more admins from **Admin → Users**.

If you are logged in but see no Admin button, run in Supabase **SQL Editor**:
`update profiles set role='admin' where email='you@example.com';`

## Environment variables
| Name | Meaning |
|---|---|
| `PORT` | Set by Render. The app never hardcodes a port. |
| `DATABASE_URL` | Supabase Transaction pooler URL (port 6543). |
| `REDIS_URL` | Upstash `rediss://` TCP URL. |
| `SUPABASE_URL` | `https://xxxx.supabase.co` |
| `SUPABASE_ANON_KEY` | Supabase anon public key. |
| `ADMIN_EMAIL` | Account that becomes admin on first verified login. |

If one is missing the app exits at start and the Render log lists every missing name.

## Check it works
- `https://YOUR-URL/healthz` shows `{"status":"ok"}`.
- Log in as a second (member) account and bid in one window while watching the price update live in another.
- Admin → Dashboard shows DB and Redis `ok`.

## Run locally
```bash
cp .env.example .env        # fill in real values
set -a; source .env; set +a
go run ./cmd/server         # http://localhost:8080
go test -count=1 ./...      # add -race if you have gcc (CGO_ENABLED=1)
```
Or fully local data stores (login still uses your Supabase project): `docker compose up --build`. This file is for development only; production uses `render.yaml`.

## How it stays correct
- Bids: one atomic SQL `UPDATE` decides the winner; the bid row is inserted in the same transaction; `(auction_id, idempotency_key)` is unique so retries never duplicate.
- Anti-sniping: a bid in the last 30 seconds extends the end time inside the same statement.
- A background worker closes expired auctions under a Redis lock; the close itself is `UPDATE ... WHERE status='active'`, so exactly one winner is recorded even with several instances.
- Every change is published to Redis `auction:{id}`; each instance forwards it to its own WebSocket clients.
- Roles and bans are read from the database on every request, so they apply instantly. Admins cannot bid.
- Admin changes require a CSRF token and are written to the audit log.

## Troubleshooting
| Symptom | Fix |
|---|---|
| Render log: `missing required environment variables` | Add the listed variables in Render → Environment. |
| `ping postgres` / timeout | Use the **Transaction pooler** URL (port 6543); re-check the password encoding. |
| `ping redis` / `redis subscribe` | Use the TCP `rediss://` URL, not REST. |
| `load Supabase JWKS` | Check `SUPABASE_URL` (no trailing path). The project must use JWT signing keys. |
| "email rate limit exceeded" on sign-up | Set up custom SMTP (Step 1.5), or add test users by hand in Supabase → Authentication → Users. |
| Everything is slow after a pause | Free-tier cold start; wait ~50 seconds. |
| "Your account has been banned" | `update profiles set banned=false where email='…';` |
| Upstash quota warnings | The app is built to use few commands (health checks hit Redis at most once a minute). Check the plan limit in Upstash. |

## Layout
```
cmd/server/main.go        entry point, routing, graceful shutdown
internal/config           env vars, fail-fast
internal/store            Postgres + Redis, migrations, seed
internal/auth             Supabase JWT check, roles, bans
internal/auction          auction model, events, closer worker
internal/bid              placing bids, rate limit, concurrency tests
internal/ws               WebSocket hub (Redis pub/sub)
internal/admin            /api/admin/*, CSRF, audit log
internal/httpx            typed errors, logging, recovery, security headers
migrations/               SQL, applied automatically at boot
web/                      index.html, style.css, app.js, admin.html, admin.js
Dockerfile, .dockerignore, render.yaml, docker-compose.yml (local only)
```
