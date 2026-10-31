package tools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

func init() {
	register(Provider{Name: "ebay", Title: "eBay", SignUp: "https://developer.ebay.com/my/keys", KeyHint: "AppID:CertID"})
	item := []Field{
		{Name: "id", Type: "string"}, {Name: "title", Type: "string"}, {Name: "price", Type: "number"}, {Name: "currency", Type: "string"},
		{Name: "original_price", Type: "number", Desc: "When it's on sale."}, {Name: "condition", Type: "string"},
		{Name: "image", Type: "string"}, {Name: "url", Type: "string"}, {Name: "seller", Type: "string"},
		{Name: "seller_feedback", Type: "number", Desc: "Percent positive."}, {Name: "shipping", Type: "number", Desc: "Lowest shipping cost, 0 for free, absent when unknown."},
		{Name: "auction", Type: "boolean"}, {Name: "bids", Type: "integer"}, {Name: "ends", Type: "string"},
		{Name: "location", Type: "string"}, {Name: "category", Type: "string"},
	}
	marketplace := Field{Name: "marketplace", Type: "string", Enum: []string{"EBAY_US", "EBAY_GB", "EBAY_DE", "EBAY_CA", "EBAY_AU"}, Desc: "Default EBAY_US."}
	define(&Tool{
		Name:    "shopping.search",
		Summary: "Search products for sale.",
		Input: []Field{
			{Name: "query", Type: "string", Required: true},
			{Name: "limit", Type: "integer", Min: 1, Max: 100, Desc: "Items to return, default 20."},
			{Name: "offset", Type: "integer", Min: 0, Max: 9900},
			{Name: "min_price", Type: "number"}, {Name: "max_price", Type: "number"},
			{Name: "condition", Type: "string", Enum: []string{"new", "refurbished", "used"}},
			{Name: "sort", Type: "string", Enum: []string{"best", "price_asc", "price_desc", "newest", "ending"}, Desc: "Default best."},
			{Name: "free_shipping", Type: "boolean"},
			marketplace,
		},
		Output:  []Field{{Name: "total", Type: "integer"}, {Name: "items", Type: "array", Items: &Field{Type: "object", Fields: item}}},
		Cache:   15 * time.Minute,
		Example: `{"query":"mac mini","limit":1}`,
		Order:   []string{"ebay"},
		Run:     map[string]RunFunc{"ebay": ebaySearch},
	})
	define(&Tool{
		Name:    "shopping.item",
		Summary: "Full details of one product from shopping.search.",
		Input:   []Field{{Name: "id", Type: "string", Required: true}, marketplace},
		Output: append(item,
			Field{Name: "images", Type: "array", Items: &Field{Type: "string"}},
			Field{Name: "description", Type: "string"},
			Field{Name: "aspects", Type: "object", Desc: "Attributes like Brand and Size."},
			Field{Name: "returns", Type: "string"}),
		Cache: 15 * time.Minute,
		Order: []string{"ebay"},
		Run:   map[string]RunFunc{"ebay": ebayItem},
	})
}

type product struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	Price         float64  `json:"price"`
	Currency      string   `json:"currency"`
	OriginalPrice *float64 `json:"original_price,omitempty"`
	Condition     string   `json:"condition,omitempty"`
	Image         string   `json:"image,omitempty"`
	URL           string   `json:"url"`
	Seller        string   `json:"seller,omitempty"`
	Feedback      *float64 `json:"seller_feedback,omitempty"`
	Shipping      *float64 `json:"shipping,omitempty"`
	Auction       bool     `json:"auction"`
	Bids          int      `json:"bids,omitempty"`
	Ends          string   `json:"ends,omitempty"`
	Location      string   `json:"location,omitempty"`
	Category      string   `json:"category,omitempty"`
}

type productDetail struct {
	product
	Images      []string          `json:"images"`
	Description string            `json:"description,omitempty"`
	Aspects     map[string]string `json:"aspects,omitempty"`
	Returns     string            `json:"returns,omitempty"`
}

type ebayMoney struct {
	Value    string `json:"value"`
	Currency string `json:"currency"`
}

func (m *ebayMoney) number() (float64, bool) {
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseFloat(m.Value, 64)
	return n, err == nil
}

type ebayImage struct {
	ImageURL string `json:"imageUrl"`
}

// ebayItemFields is what search results and item details share.
type ebayItemFields struct {
	ItemID          string     `json:"itemId"`
	Title           string     `json:"title"`
	Price           *ebayMoney `json:"price"`
	CurrentBidPrice *ebayMoney `json:"currentBidPrice"`
	MarketingPrice  *struct {
		OriginalPrice *ebayMoney `json:"originalPrice"`
	} `json:"marketingPrice"`
	Image           *ebayImage  `json:"image"`
	ThumbnailImages []ebayImage `json:"thumbnailImages"`
	Condition       string      `json:"condition"`
	ItemWebURL      string      `json:"itemWebUrl"`
	Seller          *struct {
		Username string `json:"username"`
		Feedback string `json:"feedbackPercentage"`
	} `json:"seller"`
	ShippingOptions []struct {
		ShippingCost *ebayMoney `json:"shippingCost"`
	} `json:"shippingOptions"`
	BuyingOptions []string `json:"buyingOptions"`
	BidCount      int      `json:"bidCount"`
	ItemEndDate   string   `json:"itemEndDate"`
	ItemLocation  *struct {
		City    string `json:"city"`
		State   string `json:"stateOrProvince"`
		Country string `json:"country"`
	} `json:"itemLocation"`
	Categories []struct {
		Name string `json:"categoryName"`
	} `json:"categories"`
}

func (it *ebayItemFields) product() product {
	p := product{ID: it.ItemID, Title: it.Title, Condition: it.Condition, URL: it.ItemWebURL, Bids: it.BidCount, Ends: it.ItemEndDate}
	m := it.Price
	if m == nil {
		m = it.CurrentBidPrice
	}
	if n, ok := m.number(); ok {
		p.Price, p.Currency = n, m.Currency
	}
	if it.MarketingPrice != nil {
		if n, ok := it.MarketingPrice.OriginalPrice.number(); ok && n > p.Price {
			p.OriginalPrice = &n
		}
	}
	if it.Image != nil {
		p.Image = it.Image.ImageURL
	} else if len(it.ThumbnailImages) > 0 {
		p.Image = it.ThumbnailImages[0].ImageURL
	}
	if it.Seller != nil {
		p.Seller = it.Seller.Username
		if n, err := strconv.ParseFloat(it.Seller.Feedback, 64); err == nil {
			p.Feedback = &n
		}
	}
	for _, s := range it.ShippingOptions {
		if n, ok := s.ShippingCost.number(); ok && (p.Shipping == nil || n < *p.Shipping) {
			p.Shipping = &n
		}
	}
	fixed := false
	for _, b := range it.BuyingOptions {
		switch b {
		case "AUCTION":
			p.Auction = true
		case "FIXED_PRICE":
			fixed = true
		}
	}
	p.Auction = p.Auction && !fixed
	if l := it.ItemLocation; l != nil {
		var parts []string
		for _, s := range []string{l.City, l.State, l.Country} {
			if s != "" {
				parts = append(parts, s)
			}
		}
		p.Location = strings.Join(parts, ", ")
	}
	if len(it.Categories) > 0 {
		p.Category = it.Categories[0].Name
	}
	return p
}

// ebayAuth caches the client-credentials token, which lasts about two
// hours and costs a call of its own.
var ebayAuth struct {
	sync.Mutex
	key     string
	token   string
	expires time.Time
}

func ebayToken(ctx context.Context, c *Call) (string, error) {
	ebayAuth.Lock()
	defer ebayAuth.Unlock()
	if ebayAuth.key == c.Key && time.Now().Before(ebayAuth.expires) {
		return ebayAuth.token, nil
	}
	id, secret, ok := strings.Cut(c.Key, ":")
	if !ok {
		return "", errorf(http.StatusServiceUnavailable, "bad_key", "the eBay key must be AppID:CertID (berth tools:key ebay)")
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {"https://api.ebay.com/oauth/api_scope"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url("https://api.ebay.com/identity/v1/oauth2/token"), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(id, secret)
	var r struct {
		Token   string `json:"access_token"`
		Expires int    `json:"expires_in"`
	}
	if err := c.do(req, &r); err != nil {
		return "", fmt.Errorf("sign-in %w", err)
	}
	ebayAuth.key, ebayAuth.token = c.Key, r.Token
	ebayAuth.expires = time.Now().Add(time.Duration(r.Expires)*time.Second - time.Minute)
	return r.Token, nil
}

func ebayGet(ctx context.Context, c *Call, marketplace, path string, into any) error {
	token, err := ebayToken(ctx, c)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url("https://api.ebay.com/buy/browse/v1/"+path), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if marketplace == "" {
		marketplace = "EBAY_US"
	}
	req.Header.Set("X-EBAY-C-MARKETPLACE-ID", marketplace)
	err = c.do(req, into)
	var se *statusError
	if errors.As(err, &se) && se.Status == http.StatusUnauthorized {
		ebayAuth.Lock()
		ebayAuth.expires = time.Time{}
		ebayAuth.Unlock()
	}
	return err
}

var ebayCurrency = map[string]string{"EBAY_US": "USD", "EBAY_GB": "GBP", "EBAY_DE": "EUR", "EBAY_CA": "CAD", "EBAY_AU": "AUD"}

var ebayConditions = map[string]string{
	"new":         "1000|1500|1750",
	"refurbished": "2000|2010|2020|2030|2500",
	"used":        "2750|3000|4000|5000|6000",
}

var ebaySorts = map[string]string{"price_asc": "price", "price_desc": "-price", "newest": "newlyListed", "ending": "endingSoonest"}

func ebaySearch(ctx context.Context, c *Call, args map[string]any) (any, error) {
	market := str(args, "marketplace")
	var filters []string
	lo, hasLo := args["min_price"].(float64)
	hi, hasHi := args["max_price"].(float64)
	if hasLo || hasHi {
		a, b := "", ""
		if hasLo {
			a = strconv.FormatFloat(lo, 'f', 2, 64)
		}
		if hasHi {
			b = strconv.FormatFloat(hi, 'f', 2, 64)
		}
		cur := ebayCurrency[market]
		if cur == "" {
			cur = "USD"
		}
		filters = append(filters, "price:["+a+".."+b+"]", "priceCurrency:"+cur)
	}
	if ids := ebayConditions[str(args, "condition")]; ids != "" {
		filters = append(filters, "conditionIds:{"+ids+"}")
	}
	if free, _ := args["free_shipping"].(bool); free {
		filters = append(filters, "maxDeliveryCost:0")
	}
	q := url.Values{"q": {str(args, "query")}, "limit": {strconv.Itoa(intArg(args, "limit", 20))}, "offset": {strconv.Itoa(intArg(args, "offset", 0))}}
	if filters != nil {
		q.Set("filter", strings.Join(filters, ","))
	}
	if s := ebaySorts[str(args, "sort")]; s != "" {
		q.Set("sort", s)
	}
	var r struct {
		Total int              `json:"total"`
		Items []ebayItemFields `json:"itemSummaries"`
	}
	if err := ebayGet(ctx, c, market, "item_summary/search?"+q.Encode(), &r); err != nil {
		return nil, err
	}
	out := struct {
		Total int       `json:"total"`
		Items []product `json:"items"`
	}{r.Total, []product{}}
	for i := range r.Items {
		out.Items = append(out.Items, r.Items[i].product())
	}
	return out, nil
}

func ebayItem(ctx context.Context, c *Call, args map[string]any) (any, error) {
	id := str(args, "id")
	var r struct {
		ebayItemFields
		AdditionalImages []ebayImage `json:"additionalImages"`
		Description      string      `json:"description"`
		ShortDescription string      `json:"shortDescription"`
		Aspects          []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"localizedAspects"`
		ReturnTerms *struct {
			Accepted bool `json:"returnsAccepted"`
			Period   *struct {
				Value int    `json:"value"`
				Unit  string `json:"unit"`
			} `json:"returnPeriod"`
		} `json:"returnTerms"`
	}
	err := ebayGet(ctx, c, str(args, "marketplace"), "item/"+url.PathEscape(id), &r)
	var se *statusError
	if errors.As(err, &se) && (se.Status == http.StatusNotFound || se.Status == http.StatusBadRequest) {
		return nil, notFound("no item %s", id)
	}
	if err != nil {
		return nil, err
	}
	out := productDetail{product: r.product(), Images: []string{}, Aspects: map[string]string{}}
	if out.Image != "" {
		out.Images = append(out.Images, out.Image)
	}
	for _, i := range r.AdditionalImages {
		out.Images = append(out.Images, i.ImageURL)
	}
	out.Description = r.ShortDescription
	if r.Description != "" {
		_, out.Description = readable([]byte(r.Description))
	}
	for _, a := range r.Aspects {
		out.Aspects[a.Name] = a.Value
	}
	if t := r.ReturnTerms; t != nil {
		out.Returns = "no returns"
		if t.Accepted {
			out.Returns = "returns accepted"
			if t.Period != nil {
				out.Returns = fmt.Sprintf("%d %s returns", t.Period.Value, strings.TrimPrefix(strings.ToLower(t.Period.Unit), "calendar_"))
			}
		}
	}
	return out, nil
}
