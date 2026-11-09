// Package epay is the epay (易支付 / 彩虹易支付) provider of admin.payment:
// the merchant protocol many Chinese gateways speak to take Alipay and
// WeChat Pay.
//
// An order is sent to <gateway>/submit.php (the user's browser goes there:
// the default) or created server-side at <gateway>/mapi.php (the gateway
// answers JSON with payurl, qrcode or urlscheme) with pid, type,
// out_trade_no (the order id), notify_url, return_url, name and money
// (yuan, two decimals), signed with MD5: every parameter except sign,
// sign_type and empty ones, sorted by name, joined as k=v&k=v without
// URL-encoding, the merchant key appended directly, lowercase hex; plus
// sign_type=MD5. The gateway's asynchronous notify (GET with the same
// fields plus trade_no and trade_status) is verified the same way, the pid
// must be ours and trade_status TRADE_SUCCESS; the reply is "success". The
// user's return to return_url carries the same signed parameters. epay has
// no currency field: amounts are yuan, so only a CNY site can use it.
// References: https://pay.yzfpay.com/doc/v1_legacy_api.html (submit.php,
// mapi.php, the MD5 signature, notify and return), the open-source
// 彩虹易支付 code it documents
// (https://github.com/maajiko/epay: mapi.php, includes/lib/Payment.php
// getSignContent/makeSign, includes/functions.php creat_callback), and
// new-api's client (https://github.com/QuantumNous/new-api
// controller/topup.go with github.com/Calcium-Ion/go-epay epay/util.go).
package epay

import (
	"context"
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/initxy/plugrelay/pkg/plugin"
	"github.com/initxy/plugrelay/plugins/admin/payment"
)

// Name is the provider name in admin.payment routes.
const Name = "epay"

// Currency is the only currency epay charges in.
const Currency = "CNY"

func init() {
	payment.RegisterProvider(Name, factory,
		plugin.Setting{Key: "epay_url", Type: plugin.TypeString, Scopes: plugin.Global, Merge: plugin.Override,
			Label: plugin.T("epay gateway address", "易支付网关地址"),
			Help:  plugin.T("Base address such as https://pay.example.com; submit.php and mapi.php live under it.", "例如 https://pay.example.com，submit.php 和 mapi.php 都在它下面。")},
		plugin.Setting{Key: "epay_pid", Type: plugin.TypeString, Scopes: plugin.Global, Merge: plugin.Override,
			Label: plugin.T("epay merchant ID", "易支付商户号"),
			Help:  plugin.T("The pid the gateway gave you.", "网关分配的 pid。")},
		plugin.Setting{Key: "epay_key", Type: plugin.TypeSecret, Scopes: plugin.Global, Merge: plugin.Override,
			Label: plugin.T("epay merchant key", "易支付商户密钥"),
			Help:  plugin.T("The MD5 signing key from the gateway.", "网关给的 MD5 签名密钥。")},
		plugin.Setting{Key: "epay_types", Type: plugin.TypeList, Default: []string{"alipay", "wxpay"}, Scopes: plugin.Global,
			Merge: plugin.Override, Label: plugin.T("Payment methods offered", "提供的支付方式"),
			Help: plugin.T("The gateway's type values, such as alipay, wxpay or qqpay.", "填网关的 type 值，例如 alipay、wxpay、qqpay。")},
		plugin.Setting{Key: "epay_mode", Type: plugin.TypeString, Default: "submit", Enum: []string{"submit", "mapi"},
			Scopes: plugin.Global, Merge: plugin.Override,
			Label:      plugin.T("How to start a payment", "下单方式"),
			Help:       plugin.T("Send the user to the gateway's page, or create the order through its API and send the user to the link it returns.", "把用户带到网关的支付页（submit），或者先通过接口下单再跳到返回的支付链接（mapi）。"),
			EnumLabels: map[string]plugin.Text{"submit": plugin.T("Gateway page", "跳转网关页面"), "mapi": plugin.T("Order through the API", "接口下单")}},
		plugin.Setting{Key: "epay_product_name", Type: plugin.TypeString, Default: "Balance top-up", Scopes: plugin.Global,
			Merge: plugin.Override, Label: plugin.T("Product name on the payment page", "支付页显示的商品名"),
			Help: plugin.T("Must not contain &.", "不能包含 & 符号。")},
	)
}

func factory(cfg payment.Config) (payment.Provider, bool) {
	get := func(k string) string {
		if cfg.Settings == nil {
			return ""
		}
		return cfg.Settings.Get(k).String()
	}
	p := &Provider{URL: get("epay_url"), PID: get("epay_pid"), Key: get("epay_key"), Mode: get("epay_mode"),
		Product: get("epay_product_name"), Client: cfg.Client}
	if cfg.Settings != nil {
		p.Types = cfg.Settings.Get("epay_types").List()
	}
	return p, p.URL != "" && p.PID != "" && p.Key != ""
}

// Provider implements payment.Provider and payment.FormProvider.
type Provider struct {
	URL, PID, Key string
	Types         []string
	Mode          string
	Product       string
	Client        *http.Client
}

var (
	_ payment.Provider         = (*Provider)(nil)
	_ payment.FormProvider     = (*Provider)(nil)
	_ payment.MethodProvider   = (*Provider)(nil)
	_ payment.CurrencyProvider = (*Provider)(nil)
)

// Methods implements payment.MethodProvider.
func (p *Provider) Methods() []string {
	if len(p.Types) == 0 {
		return []string{"alipay", "wxpay"}
	}
	return p.Types
}

// Currencies implements payment.CurrencyProvider.
func (p *Provider) Currencies() []string { return []string{Currency} }

// Sign is the epay MD5 signature of params with key.
func Sign(params url.Values, key string) string {
	names := make([]string, 0, len(params))
	for k := range params {
		if k == "sign" || k == "sign_type" || params.Get(k) == "" {
			continue
		}
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for i, k := range names {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k + "=" + params.Get(k))
	}
	sum := md5.Sum([]byte(b.String() + key))
	return hex.EncodeToString(sum[:])
}

// Yuan formats micro-units as epay's money (yuan with two decimals);
// ok=false for an amount that is not a whole number of fen.
func Yuan(micro int64) (string, bool) {
	if micro <= 0 || micro%10_000 != 0 {
		return "", false
	}
	fen := micro / 10_000
	return fmt.Sprintf("%d.%02d", fen/100, fen%100), true
}

// parseYuan reads a money parameter ("10", "10.5", "10.00") exactly; the
// gateway may drop trailing zeros.
func parseYuan(s string) (int64, bool) {
	whole, frac, _ := strings.Cut(strings.TrimSpace(s), ".")
	if whole == "" || len(frac) > 6 || strings.Trim(whole+frac, "0123456789") != "" {
		return 0, false
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > 1<<40 {
		return 0, false
	}
	f := int64(0)
	if frac != "" {
		f, _ = strconv.ParseInt((frac + "000000")[:6], 10, 64)
	}
	return w*1_000_000 + f, true
}

// Checkout implements payment.Provider.
func (p *Provider) Checkout(ctx context.Context, o payment.Order, _, _ string) (payment.Session, error) {
	if !strings.EqualFold(o.Currency, Currency) {
		return payment.Session{}, fmt.Errorf("epay charges in CNY only; the site currency is %s", o.Currency)
	}
	money, ok := Yuan(o.AmountMicro)
	if !ok {
		return payment.Session{}, fmt.Errorf("amount %d micro-units is not a whole number of fen", o.AmountMicro)
	}
	if o.CallbackURL == "" {
		return payment.Session{}, errors.New("epay: no callback URL for the order")
	}
	typ := o.Method
	if typ == "" {
		typ = p.Methods()[0]
	}
	name := p.Product
	if name == "" {
		name = "Balance top-up"
	}
	v := url.Values{}
	v.Set("pid", p.PID)
	v.Set("type", typ)
	v.Set("out_trade_no", o.ID)
	v.Set("notify_url", o.CallbackURL+"/webhook/"+Name)
	v.Set("return_url", o.CallbackURL+"/return/"+Name)
	v.Set("name", name)
	v.Set("money", money)
	if p.Mode == "mapi" {
		v.Set("clientip", o.ClientIP)
		v.Set("device", "pc")
	}
	// Values are signed unencoded, so a '&' would make the signature
	// ambiguous; gateways reject them.
	for k := range v {
		if strings.ContainsAny(v.Get(k), "&") {
			return payment.Session{}, fmt.Errorf("epay: %s must not contain '&'", k)
		}
	}
	v.Set("sign", Sign(v, p.Key))
	v.Set("sign_type", "MD5")
	base := strings.TrimRight(p.URL, "/")
	if p.Mode != "mapi" {
		return payment.Session{ID: o.ID, URL: base + "/submit.php?" + v.Encode()}, nil
	}
	return p.mapi(ctx, base, v)
}

func (p *Provider) mapi(ctx context.Context, base string, v url.Values) (payment.Session, error) {
	if p.Client == nil {
		return payment.Session{}, errors.New("no HTTP client")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/mapi.php", strings.NewReader(v.Encode()))
	if err != nil {
		return payment.Session{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.Client.Do(req)
	if err != nil {
		return payment.Session{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Code      json.RawMessage `json:"code"`
		Msg       string          `json:"msg"`
		TradeNo   string          `json:"trade_no"`
		PayURL    string          `json:"payurl"`
		QRCode    string          `json:"qrcode"`
		URLScheme string          `json:"urlscheme"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return payment.Session{}, fmt.Errorf("epay: unreadable mapi.php answer (%s)", resp.Status)
	}
	if strings.Trim(string(out.Code), `"`) != "1" {
		return payment.Session{}, fmt.Errorf("epay: %s", out.Msg)
	}
	u := out.PayURL
	if u == "" {
		u = out.QRCode
	}
	if u == "" {
		u = out.URLScheme
	}
	if u == "" {
		return payment.Session{}, errors.New("epay: mapi.php returned no payment URL")
	}
	return payment.Session{ID: out.TradeNo, URL: u}, nil
}

// Verify implements payment.Provider for a form-encoded POST body.
func (p *Provider) Verify(_ http.Header, body []byte, now time.Time) (payment.Event, error) {
	v, err := url.ParseQuery(string(body))
	if err != nil {
		return payment.Event{}, fmt.Errorf("%w: unreadable callback", payment.ErrBadSignature)
	}
	return p.VerifyForm(v, now)
}

// VerifyForm implements payment.FormProvider.
func (p *Provider) VerifyForm(v url.Values, _ time.Time) (payment.Event, error) {
	got := strings.ToLower(v.Get("sign"))
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(Sign(v, p.Key))) != 1 {
		return payment.Event{}, fmt.Errorf("%w: epay sign mismatch", payment.ErrBadSignature)
	}
	if st := v.Get("sign_type"); st != "" && !strings.EqualFold(st, "MD5") {
		return payment.Event{}, fmt.Errorf("%w: sign_type %q", payment.ErrBadSignature, st)
	}
	if v.Get("pid") != p.PID {
		return payment.Event{}, fmt.Errorf("%w: callback for another merchant", payment.ErrBadSignature)
	}
	id := v.Get("trade_no")
	if id == "" {
		id = v.Get("out_trade_no")
	}
	ev := payment.Event{ID: id, OrderID: v.Get("out_trade_no"), Paid: v.Get("trade_status") == "TRADE_SUCCESS", Currency: Currency}
	// An unreadable amount reads as zero: never equal to an order, so it is
	// logged and not credited.
	ev.AmountMicro, _ = parseYuan(v.Get("money"))
	return ev, nil
}

// Ack implements payment.FormProvider: epay retries until it reads
// "success".
func (p *Provider) Ack(ok bool) string {
	if ok {
		return "success"
	}
	return "fail"
}
