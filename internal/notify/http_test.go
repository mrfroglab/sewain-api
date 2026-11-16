package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestAPI(t *testing.T) (*API, http.Handler) {
	t.Helper()
	db := testPool(t)
	addUser(t, db, "u-a", "a@contoh.id")
	addUser(t, db, "u-b", "")
	a := &API{
		DB:             db,
		Viewer:         func(r *http.Request) string { return r.Header.Get("X-Test-User") },
		VAPIDPublicKey: "BPublicKeyForTests",
		Available:      map[Channel]bool{ChannelWebPush: true, ChannelEmail: true},
	}
	mux := http.NewServeMux()
	a.Register(mux)
	return a, mux
}

func call(t *testing.T, h http.Handler, method, path, user, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if user != "" {
		req.Header.Set("X-Test-User", user)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: body %q", method, path, rec.Body.String())
	}
	return rec.Code, out
}

func subBody(endpoint string) string {
	return `{"endpoint":"` + endpoint + `","expirationTime":null,"keys":{"p256dh":"` + testP256dh + `","auth":"` + testAuth + `"}}`
}

func TestPushEndpoints(t *testing.T) {
	a, h := newTestAPI(t)
	code, out := call(t, h, "GET", "/api/v1/push/vapid-key", "", "")
	if code != 200 || out["publicKey"] != "BPublicKeyForTests" {
		t.Fatalf("vapid-key: %d %v", code, out)
	}

	ep := "https://fcm.googleapis.com/fcm/send/dEvIcE-1"
	if code, out := call(t, h, "POST", "/api/v1/push/subscriptions", "", subBody(ep)); code != 401 || out["error"] != "Masuk dulu, ya." {
		t.Fatalf("guest subscribe: %d %v", code, out)
	}
	for name, body := range map[string]string{
		"foreign host":  subBody("https://evil.example/collect"),
		"plain http":    subBody("http://fcm.googleapis.com/fcm/send/x"),
		"bad keys":      `{"endpoint":"` + ep + `","keys":{"p256dh":"abc","auth":"def"}}`,
		"not json":      `{`,
		"look-alike":    subBody("https://fcm.googleapis.com.evil.example/x"),
		"userinfo host": subBody("https://fcm.googleapis.com@evil.example/x"),
	} {
		if code, out := call(t, h, "POST", "/api/v1/push/subscriptions", "u-a", body); code != 400 || out["ok"] != false {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if code, out := call(t, h, "POST", "/api/v1/push/subscriptions", "u-a", subBody(ep)); code != 201 || out["ok"] != true {
		t.Fatalf("subscribe: %d %v", code, out)
	}
	if n := subscriptionCount(t, a.DB, "u-a"); n != 1 {
		t.Fatalf("u-a subscriptions = %d", n)
	}
	// Same browser, other account signs in: the subscription moves.
	if code, _ := call(t, h, "POST", "/api/v1/push/subscriptions", "u-b", subBody(ep)); code != 201 {
		t.Fatal("re-subscribe failed")
	}
	if subscriptionCount(t, a.DB, "u-a") != 0 || subscriptionCount(t, a.DB, "u-b") != 1 {
		t.Fatal("subscription did not move to the new user")
	}
	// u-a cannot delete u-b's subscription.
	if code, out := call(t, h, "DELETE", "/api/v1/push/subscriptions", "u-a", `{"endpoint":"`+ep+`"}`); code != 200 || out["removed"] != false {
		t.Fatalf("foreign delete: %d %v", code, out)
	}
	if code, out := call(t, h, "DELETE", "/api/v1/push/subscriptions", "u-b", `{"endpoint":"`+ep+`"}`); code != 200 || out["removed"] != true {
		t.Fatalf("delete: %d %v", code, out)
	}

	// Oldest subscriptions are trimmed beyond the per-user limit.
	for i := range MaxSubscriptionsPerUser + 3 {
		call(t, h, "POST", "/api/v1/push/subscriptions", "u-a", subBody("https://web.push.apple.com/dev-"+string(rune('a'+i))))
	}
	if n := subscriptionCount(t, a.DB, "u-a"); n != MaxSubscriptionsPerUser {
		t.Fatalf("subscriptions after trim = %d", n)
	}

	// Push disabled on the server.
	a.VAPIDPublicKey = ""
	if code, _ := call(t, h, "GET", "/api/v1/push/vapid-key", "", ""); code != 404 {
		t.Fatalf("disabled vapid-key: %d", code)
	}
	if code, _ := call(t, h, "POST", "/api/v1/push/subscriptions", "u-a", subBody(ep)); code != 404 {
		t.Fatalf("disabled subscribe: %d", code)
	}
}

func TestPrefsEndpoints(t *testing.T) {
	_, h := newTestAPI(t)
	if code, _ := call(t, h, "GET", "/api/v1/me/notification-prefs", "", ""); code != 401 {
		t.Fatalf("guest: %d", code)
	}
	code, out := call(t, h, "GET", "/api/v1/me/notification-prefs", "u-a", "")
	if code != 200 {
		t.Fatalf("get: %d %v", code, out)
	}
	prefs := out["prefs"].(map[string]any)
	ch := prefs["channels"].(map[string]any)
	if ch["email"].(map[string]any)["booking"] != false || ch["email"].(map[string]any)["sengketa"] != true ||
		ch["inapp"].(map[string]any)["chat"] != true || ch["whatsapp"].(map[string]any)["sistem"] != false {
		t.Fatalf("defaults: %v", ch)
	}
	q := prefs["quietHours"].(map[string]any)
	if q["start"] != "22:00" || q["end"] != "06:00" || q["timezone"] != "Asia/Jakarta" || q["enabled"] != true {
		t.Fatalf("quiet hours: %v", q)
	}
	avail := out["available"].(map[string]any)
	if avail["webpush"] != true || avail["whatsapp"] != false || avail["inapp"] != true || out["pushSubscriptions"] != float64(0) {
		t.Fatalf("available: %v / %v", avail, out["pushSubscriptions"])
	}

	code, out = call(t, h, "PUT", "/api/v1/me/notification-prefs", "u-a",
		`{"channels":{"email":{"booking":true},"webpush":{"chat":false}},"quietHours":{"enabled":false,"end":"07:30"}}`)
	if code != 200 {
		t.Fatalf("put: %d %v", code, out)
	}
	code, out = call(t, h, "GET", "/api/v1/me/notification-prefs", "u-a", "")
	ch = out["prefs"].(map[string]any)["channels"].(map[string]any)
	q = out["prefs"].(map[string]any)["quietHours"].(map[string]any)
	if code != 200 || ch["email"].(map[string]any)["booking"] != true || ch["webpush"].(map[string]any)["chat"] != false ||
		ch["webpush"].(map[string]any)["booking"] != true || q["enabled"] != false || q["end"] != "07:30" || q["start"] != "22:00" {
		t.Fatalf("after put: %v %v", ch, q)
	}
	// Other users are unaffected.
	_, out = call(t, h, "GET", "/api/v1/me/notification-prefs", "u-b", "")
	if out["prefs"].(map[string]any)["channels"].(map[string]any)["email"].(map[string]any)["booking"] != false {
		t.Fatal("prefs leaked across users")
	}

	for name, body := range map[string]string{
		"bad clock":    `{"quietHours":{"start":"7 pagi"}}`,
		"bad timezone": `{"quietHours":{"timezone":"Mars/Olympus"}}`,
		"bad channel":  `{"channels":{"sms":{"booking":true}}}`,
		"bad category": `{"channels":{"email":{"promo":true}}}`,
	} {
		if code, out := call(t, h, "PUT", "/api/v1/me/notification-prefs", "u-a", body); code != 400 || out["error"] == "" {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
}
