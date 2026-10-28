package newsletter

import (
	"bytes"
	"html/template"
)

const confirmationSubject = "Confirm your subscription to Davide Imola's newsletter"

// The confirmation email. Inline styles only, because that is what mail clients keep, and the
// brand's paper and ink. No image of any kind: the email this service sends to ask for
// consent is the last place a pixel could hide.
var confirmationTemplate = template.Must(template.New("confirmation").Parse(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Confirm your subscription</title></head>
<body style="margin:0;padding:0;background:#F7F5F0;">
<div style="max-width:520px;margin:0 auto;padding:40px 24px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif;color:#1C2233;font-size:16px;line-height:1.6;">
<p style="margin:0 0 24px;font-weight:700;font-size:18px;">Davide Imola</p>
<p style="margin:0 0 16px;">Someone, hopefully you, asked to receive my newsletter at this address. It comes once a month: what I wrote, where I am speaking, what I am building.</p>
<p style="margin:0 0 24px;">To start receiving it, confirm the address:</p>
<p style="margin:0 0 24px;"><a href="{{.ConfirmURL}}" style="display:inline-block;background:#1C2233;color:#F7F5F0;text-decoration:none;padding:12px 20px;border-radius:4px;font-weight:600;">Confirm my subscription</a></p>
<p style="margin:0 0 16px;font-size:14px;color:#3B4152;">Or paste this link into your browser:<br><a href="{{.ConfirmURL}}" style="color:#1C2233;word-break:break-all;">{{.ConfirmURL}}</a></p>
<p style="margin:0 0 16px;font-size:14px;color:#3B4152;">The link works for seven days. If you did not ask for this, ignore this email: nothing happens without the click, and the address is deleted within 30 days.</p>
<p style="margin:32px 0 0;font-size:13px;color:#5B6070;">The newsletter carries no tracking pixel and no tracked links. <a href="{{.SiteURL}}/privacy" style="color:#5B6070;">Privacy</a></p>
</div>
</body>
</html>
`))

func renderConfirmation(confirmURL, siteURL string) (string, error) {
	var b bytes.Buffer
	err := confirmationTemplate.Execute(&b, struct{ ConfirmURL, SiteURL string }{confirmURL, siteURL})
	return b.String(), err
}
