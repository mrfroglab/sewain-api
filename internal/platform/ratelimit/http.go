package ratelimit

import (
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/mrfroglab/sewain-api/internal/platform/middleware"
)

// KeyFunc derives the bucket key for a request. Returning "" skips limiting for that request.
type KeyFunc func(r *http.Request) string

// IPBucket returns the rate-limit identity of a client address: IPv4 (and IPv4-mapped IPv6) addresses as
// they are, IPv6 addresses as their /64 ("2001:db8:a:b::/64"). A single IPv6 subscriber or VPS routinely
// owns a whole /64, so keying on the full address would hand an attacker 2^64 fresh buckets. Values that
// do not parse as an address are returned unchanged.
func IPBucket(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap().WithZone("")
	if a.Is4() {
		return a.String()
	}
	return netip.PrefixFrom(a, 64).Masked().String()
}

// ByIP keys on the client IP resolved by middleware.RealIP (falling back to RemoteAddr), grouping IPv6
// clients by /64 (see IPBucket).
func ByIP(r *http.Request) string {
	if ip := middleware.ClientIPFrom(r.Context()); ip != "" {
		return "ip:" + IPBucket(ip)
	}
	return "ip:" + r.RemoteAddr
}

// ByUser keys on the authenticated user (middleware.SetUserID) and falls back to the IP for guests, so
// unauthenticated traffic on a user-keyed route is still limited.
func ByUser(r *http.Request) string {
	if uid := middleware.UserIDFrom(r.Context()); uid != "" {
		return "user:" + uid
	}
	return ByIP(r)
}

// ByUserAndIP keys on the pair, e.g. to slow down one account being driven from many IPs separately.
func ByUserAndIP(r *http.Request) string {
	return ByUser(r) + "|" + ByIP(r)
}

// Profile is a named rate plus the key it applies to.
type Profile struct {
	Name string
	Rate Rate
	Key  KeyFunc
}

// Built-in profiles (F5.2) with the pilot defaults. Burst is smaller than the per-minute limit so a client
// cannot spend a whole minute's allowance in one second.
//
// The numbers are sized for a campus NAT: one Wi-Fi egress IP can carry hundreds to thousands of students
// (the UGM pilot, a registration booth), so per-IP limits must hold a few hundred people browsing at once.
// Signed-in traffic is keyed per user wherever possible, so an IP shared by many students does not share
// one bucket. Every rate can be overridden from the environment (see Rates and config.Config.Rates).
var (
	// Public: the coarse limit on every route, keyed per signed-in user and per IP for guests (ByUser).
	// 1200/min per guest IP lets a crowd of anonymous visitors behind one NAT browse; a single flooding IP
	// still hits it.
	Public = Profile{Name: "public", Rate: DefaultRates().Public, Key: ByUser}
	// Write: state-changing actions per user (guests per IP).
	Write = Profile{Name: "write", Rate: DefaultRates().Write, Key: ByUser}
	// Upload: photo and KYC uploads per user (guests per IP).
	Upload = Profile{Name: "upload", Rate: DefaultRates().Upload, Key: ByUser}
	// Auth: OTP request/verify per IP. The OTP service adds its own per-phone, per-IP and daily limits in
	// the database (auth.Config), which are the real defense against SMS pumping.
	Auth = Profile{Name: "auth", Rate: DefaultRates().Auth, Key: ByIP}
	// Demo: the DEMO_MODE persona login per IP. Generous on purpose: a demo or a regression run logs many
	// personas in from one machine, and the endpoint does not exist outside DEMO_MODE.
	Demo = Profile{Name: "demo", Rate: DefaultRates().Demo, Key: ByIP}
)

// Rates are the configurable rates of the built-in profiles.
type Rates struct {
	Public, Write, Upload, Auth, Demo Rate
}

// DefaultRates returns the pilot defaults (docs/integration/ops.md §2).
func DefaultRates() Rates {
	perMin := func(limit, burst int) Rate { return Rate{Limit: limit, Per: time.Minute, Burst: burst} }
	return Rates{
		Public: perMin(1200, 300),
		Write:  perMin(120, 40),
		Upload: perMin(30, 10),
		Auth:   perMin(60, 20),
		Demo:   perMin(600, 100),
	}
}

// WithDefaults replaces every unset rate (Limit <= 0) with its default and fills a missing Per with a minute.
func (r Rates) WithDefaults() Rates {
	d := DefaultRates()
	pick := func(v, def Rate) Rate {
		if v.Limit <= 0 {
			return def
		}
		if v.Per <= 0 {
			v.Per = time.Minute
		}
		return v
	}
	return Rates{
		Public: pick(r.Public, d.Public),
		Write:  pick(r.Write, d.Write),
		Upload: pick(r.Upload, d.Upload),
		Auth:   pick(r.Auth, d.Auth),
		Demo:   pick(r.Demo, d.Demo),
	}
}

// With returns p with rate r (the profile name and key stay).
func (p Profile) With(r Rate) Profile {
	p.Rate = r
	return p
}

// Profiles lists the built-in profiles with their default rates.
func Profiles() []Profile { return []Profile{Public, Write, Upload, Auth, Demo} }

// Options tunes Middleware.
type Options struct {
	Log *slog.Logger
	// OnLimited is called for every rejected request (e.g. to count it in metrics).
	OnLimited func(profile string, r *http.Request)
}

// Middleware enforces p with l. Rejected requests get 429 with Retry-After (whole seconds, rounded up) and
// {"ok":false,"error":"Terlalu banyak permintaan. Coba lagi dalam N detik."}. If the limiter itself fails
// the request is let through (fail open) and the error is logged: a broken limiter must not take the API
// down.
func Middleware(l Limiter, p Profile, o Options) func(http.Handler) http.Handler {
	key := p.Key
	if key == nil {
		key = ByIP
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k := key(r)
			if k == "" {
				next.ServeHTTP(w, r)
				return
			}
			d, err := l.Allow(r.Context(), p.Name+"|"+k, p.Rate)
			if err != nil {
				if o.Log != nil {
					o.Log.ErrorContext(r.Context(), "rate limiter failed", "profile", p.Name, "err", err)
				}
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Set("X-RateLimit-Limit", strconv.Itoa(d.Limit))
			h.Set("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))
			if !d.Allowed {
				secs := int(math.Ceil(d.RetryAfter.Seconds()))
				if secs < 1 {
					secs = 1
				}
				h.Set("Retry-After", strconv.Itoa(secs))
				if o.OnLimited != nil {
					o.OnLimited(p.Name, r)
				}
				middleware.WriteError(w, http.StatusTooManyRequests,
					fmt.Sprintf("Terlalu banyak permintaan. Coba lagi dalam %d detik.", secs))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
