package notify

import (
	"bytes"
	htmltemplate "html/template"
	"strings"
	texttemplate "text/template"
)

// DefaultPrefsPath is the web page where users change notification preferences (to be built by web-v2, F6.6).
const DefaultPrefsPath = "/profil/notifikasi"

// Email is a rendered notification email.
type Email struct {
	Subject string
	Text    string
	HTML    string
}

type emailData struct {
	Title, Body, URL, PrefsURL, HomeURL string
}

var textTmpl = texttemplate.Must(texttemplate.New("text").Parse(`{{.Title}}

{{.Body}}

Buka di Sewain: {{.URL}}

--
Kamu menerima email ini karena punya akun Sewain.
Atur notifikasi: {{.PrefsURL}}
`))

var htmlTmpl = htmltemplate.Must(htmltemplate.New("html").Parse(`<!doctype html>
<html lang="id">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.Title}}</title></head>
<body style="margin:0;padding:0;background:#f4f4f5;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#18181b">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#f4f4f5;padding:24px 12px">
<tr><td align="center">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:520px;background:#ffffff;border-radius:12px;padding:28px">
<tr><td style="font-size:20px;font-weight:700;color:#0f766e;padding-bottom:16px"><a href="{{.HomeURL}}" style="color:#0f766e;text-decoration:none">Sewain</a></td></tr>
<tr><td style="font-size:18px;font-weight:600;padding-bottom:8px">{{.Title}}</td></tr>
<tr><td style="font-size:15px;line-height:1.5;color:#3f3f46;padding-bottom:24px;white-space:pre-line">{{.Body}}</td></tr>
<tr><td style="padding-bottom:24px"><a href="{{.URL}}" style="display:inline-block;background:#0f766e;color:#ffffff;text-decoration:none;font-weight:600;padding:12px 20px;border-radius:8px">Buka di Sewain</a></td></tr>
<tr><td style="font-size:12px;color:#71717a;line-height:1.5;border-top:1px solid #e4e4e7;padding-top:16px">
Kamu menerima email ini karena punya akun Sewain.
<a href="{{.PrefsURL}}" style="color:#71717a">Atur notifikasi</a></td></tr>
</table>
</td></tr>
</table>
</body>
</html>
`))

// RenderEmail builds the subject (= notification title), plain text and HTML of a notification email.
func RenderEmail(p Payload, publicURL, prefsPath string) Email {
	if prefsPath == "" {
		prefsPath = DefaultPrefsPath
	}
	link := p.URL
	if link == "" {
		link = AbsURL(publicURL, p.Href)
	}
	d := emailData{
		Title: strings.TrimSpace(p.Title), Body: strings.TrimSpace(p.Body), URL: link,
		PrefsURL: AbsURL(publicURL, prefsPath), HomeURL: AbsURL(publicURL, "/"),
	}
	subject := strings.Join(strings.Fields(d.Title), " ") // no header injection through newlines
	if subject == "" {
		subject = "Notifikasi Sewain"
	}
	var t, h bytes.Buffer
	_ = textTmpl.Execute(&t, d)
	_ = htmlTmpl.Execute(&h, d)
	return Email{Subject: subject, Text: t.String(), HTML: h.String()}
}
