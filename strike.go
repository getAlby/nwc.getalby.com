package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt"
	"github.com/google/uuid"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	"gorm.io/gorm"
)

type StrikeOAuthService struct {
	cfg       *Config
	oauthConf *oauth2.Config
	db        *gorm.DB
	Logger    *logrus.Logger
}

func NewStrikeOauthService(svc *Service, e *echo.Echo) (result *StrikeOAuthService, err error) {
	conf := &oauth2.Config{
		ClientID:     svc.cfg.ClientId,
		ClientSecret: svc.cfg.ClientSecret,
		Scopes:       []string{"offline_access", "partner.account.profile.read", "partner.balances.read", "partner.invoice.read", "partner.invoice.create", "partner.invoice.quote.generate", "partner.payment-quote.lightning.create", "partner.payment-quote.execute"},
		Endpoint: oauth2.Endpoint{
			TokenURL:  svc.cfg.OAuthTokenUrl,
			AuthURL:   svc.cfg.OAuthAuthUrl,
			AuthStyle: 2, // use HTTP Basic Authorization https://pkg.go.dev/golang.org/x/oauth2#AuthStyle
		},
		RedirectURL: svc.cfg.OAuthRedirectUrl,
	}

	strikeSvc := &StrikeOAuthService{
		cfg:       svc.cfg,
		oauthConf: conf,
		db:        svc.db,
		Logger:    svc.Logger,
	}

	e.GET("/strike/auth", strikeSvc.AuthHandler)
	e.GET("/strike/callback", strikeSvc.CallbackHandler)

	return strikeSvc, err
}

func (svc *StrikeOAuthService) FetchUserToken(ctx context.Context, app App) (token *oauth2.Token, err error) {
	user := app.User
	tok, err := svc.oauthConf.TokenSource(ctx, &oauth2.Token{
		AccessToken:  user.AccessToken,
		RefreshToken: user.RefreshToken,
		Expiry:       user.Expiry,
	}).Token()
	// TODO: Parse token for id, if implementing get_info
	if err != nil {
		svc.Logger.WithFields(logrus.Fields{
			"senderPubkey": app.NostrPubkey,
			"appId":        app.ID,
			"userId":       app.User.ID,
		}).Errorf("Token error: %v", err)
		return nil, err
	}
	// we always update the user's token for future use
	// the oauth library handles the token refreshing
	user.AccessToken = tok.AccessToken
	user.RefreshToken = tok.RefreshToken
	user.Expiry = tok.Expiry // TODO; probably needs some calculation
	err = svc.db.Save(&user).Error
	if err != nil {
		svc.Logger.WithError(err).Error("Error saving user")
		return nil, err
	}
	return tok, nil
}

func (*StrikeOAuthService) GetInfo(ctx context.Context, senderPubkey string) (info *NodeInfo, err error) {
	return &NodeInfo{
		Alias:       "strike.com",
		Color:       "",
		Pubkey:      "",
		Network:     "mainnet",
		BlockHeight: 0,
		BlockHash:   "",
	}, nil
}

func (*StrikeOAuthService) SendKeysend(ctx context.Context, senderPubkey string, amount int64, destination string, preimage string, custom_records []TLVRecord) (preImage string, err error) {
	return "", errors.New("not implemented")
}

func (svc *StrikeOAuthService) ListTransactions(ctx context.Context, senderPubkey string, from, until, limit, offset uint64, unpaid bool, invoiceType string) (transactions []Nip47Transaction, err error) {
	// return empty array for now
	return []Nip47Transaction{}, nil
}

func (svc *StrikeOAuthService) LookupInvoice(ctx context.Context, senderPubkey string, paymentHash string) (transaction *Nip47Transaction, err error) {
	// TODO: move to a shared function
	app := App{}
	err = svc.db.Preload("User").First(&app, &App{
		NostrPubkey: senderPubkey,
	}).Error
	if err != nil {
		svc.Logger.WithFields(logrus.Fields{
			"senderPubkey": senderPubkey,
			"paymentHash":  paymentHash,
		}).Errorf("App not found: %v", err)
		return nil, err
	}

	svc.Logger.WithFields(logrus.Fields{
		"senderPubkey": senderPubkey,
		"paymentHash":  paymentHash,
		"appId":        app.ID,
		"userId":       app.User.ID,
	}).Info("Processing lookup invoice request")
	tok, err := svc.FetchUserToken(ctx, app)
	if err != nil {
		return nil, err
	}
	client := svc.oauthConf.Client(ctx, tok)

	// paymentHash is actually invoiceId
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/invoices/%s", svc.cfg.OAuthAPIURL, paymentHash), nil)
	if err != nil {
		svc.Logger.WithError(err).Errorf("Error creating request /invoices/%s", paymentHash)
		return nil, err
	}

	req.Header.Set("User-Agent", "NWC")

	resp, err := client.Do(req)
	if err != nil {
		svc.Logger.WithFields(logrus.Fields{
			"senderPubkey": senderPubkey,
			"appId":        app.ID,
			"userId":       app.User.ID,
		}).Errorf("Failed to lookup invoice: %v", err)
		return nil, err
	}

	if resp.StatusCode < 300 {
		responsePayload := &StrikeLookupInvoiceResponse{}
		err = json.NewDecoder(resp.Body).Decode(responsePayload)
		if err != nil {
			return nil, err
		}
		svc.Logger.WithFields(logrus.Fields{
			"senderPubkey": senderPubkey,
			"paymentHash":  paymentHash,
			"appId":        app.ID,
			"userId":       app.User.ID,
			"settled":      responsePayload.State == "PAID",
		}).Info("Lookup invoice successful")

		createdAt, _ := time.Parse(time.RFC3339, responsePayload.Created)
		amountInBTC, _ := strconv.ParseFloat(responsePayload.Amount.Amount, 64)
		transaction = &Nip47Transaction{
			// TODO: replace with bolt11 (currently not returned in response)
			Invoice:     "sampleinvoice",
			Description: responsePayload.Description,
			PaymentHash: paymentHash,
			Preimage:    "samplepreimage",
			Amount:      int64(amountInBTC * math.Pow(10, 8)),
			CreatedAt:   createdAt.Unix(),
		}

		if responsePayload.State == "PAID" {
			// TODO: replace with actual settledAt (currently not returned in response)
			timeNow := time.Now().Unix()
			transaction.SettledAt = &timeNow
		}
		fmt.Println(transaction)
		return transaction, nil
	}

	errorPayload := &StrikeErrorResponse{}
	err = json.NewDecoder(resp.Body).Decode(errorPayload)
	svc.Logger.WithFields(logrus.Fields{
		"senderPubkey":  senderPubkey,
		"paymentHash":   paymentHash,
		"appId":         app.ID,
		"userId":        app.User.ID,
		"APIHttpStatus": resp.StatusCode,
	}).Errorf("Lookup invoice failed %s", string(errorPayload.Data.Message))
	return nil, errors.New(errorPayload.Data.Message)
}

func (svc *StrikeOAuthService) MakeInvoice(ctx context.Context, senderPubkey string, amount int64, description string, descriptionHash string, expiry int64) (transaction *Nip47Transaction, err error) {
	app := App{}
	err = svc.db.Preload("User").First(&app, &App{
		NostrPubkey: senderPubkey,
	}).Error
	if err != nil {
		svc.Logger.WithFields(logrus.Fields{
			"senderPubkey":    senderPubkey,
			"amount":          amount,
			"description":     description,
			"descriptionHash": descriptionHash,
			"expiry":          expiry,
		}).Errorf("App not found: %v", err)
		return nil, err
	}

	correlationId := uuid.New()
	// amount provided in msat, but Strike API currently only supports BTC value.
	amountBTC := (float64(amount) / math.Pow(10, 11)) // 3 + 8
	if amount < 0 {
		svc.Logger.WithFields(logrus.Fields{
			"senderPubkey":    senderPubkey,
			"amount":          amount,
			"description":     description,
			"descriptionHash": descriptionHash,
			"expiry":          expiry,
		}).Errorf("amount must be 1000 msat or greater")
		return nil, errors.New("amount must be 1000 msat or greater")
	}

	svc.Logger.WithFields(logrus.Fields{
		"senderPubkey":    senderPubkey,
		"amount":          amount,
		"description":     description,
		"descriptionHash": descriptionHash,
		"expiry":          expiry,
		"appId":           app.ID,
		"userId":          app.User.ID,
	}).Info("Processing make invoice request")
	tok, err := svc.FetchUserToken(ctx, app)
	if err != nil {
		return nil, err
	}
	client := svc.oauthConf.Client(ctx, tok)

	body := bytes.NewBuffer([]byte{})
	payloadAmount := &StrikeAmount{
		Amount:   strconv.FormatFloat(amountBTC, 'f', -1, 64),
		Currency: "BTC",
	}
	payload := &StrikeInvoiceQuoteRequest{
		Amount:        *payloadAmount,
		Description:   description,
		CorrelationId: correlationId.String(),
	}
	err = json.NewEncoder(body).Encode(payload)

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/invoices", svc.cfg.OAuthAPIURL), body)
	if err != nil {
		svc.Logger.WithError(err).Error("Error creating request /invoices")
		return nil, err
	}

	req.Header.Set("User-Agent", "NWC")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		svc.Logger.WithFields(logrus.Fields{
			"senderPubkey":    senderPubkey,
			"amount":          amount,
			"description":     description,
			"descriptionHash": descriptionHash,
			"expiry":          expiry,
			"appId":           app.ID,
			"userId":          app.User.ID,
		}).Errorf("Failed to make invoice: %v", err)
		return nil, err
	}

	if resp.StatusCode >= 300 {
		errorPayload := &StrikeErrorResponse{}
		err = json.NewDecoder(resp.Body).Decode(errorPayload)
		svc.Logger.WithFields(logrus.Fields{
			"senderPubkey":    senderPubkey,
			"amount":          amount,
			"description":     description,
			"descriptionHash": descriptionHash,
			"expiry":          expiry,
			"appId":           app.ID,
			"userId":          app.User.ID,
			"APIHttpStatus":   resp.StatusCode,
		}).Errorf("Make invoice failed %s", string(errorPayload.Data.Message))
		return nil, errors.New(errorPayload.Data.Message)
	}

	responsePayload := &StrikeInvoiceQuoteResponse{}
	err = json.NewDecoder(resp.Body).Decode(responsePayload)
	if err != nil {
		return nil, err
	}

	// this is similar to paymentHash
	invoiceId := responsePayload.InvoiceId
	req, err = http.NewRequest("POST", fmt.Sprintf("%s/invoices/%s/quote", svc.cfg.OAuthAPIURL, invoiceId), nil)
	if err != nil {
		svc.Logger.WithError(err).Errorf("Error creating request /invoices/%s/quote", invoiceId)
		return nil, err
	}

	req.Header.Set("User-Agent", "NWC")
	req.Header.Set("Content-Type", "application/json")

	resp, err = client.Do(req)
	if err != nil {
		svc.Logger.WithFields(logrus.Fields{
			"senderPubkey":    senderPubkey,
			"amount":          amount,
			"description":     description,
			"descriptionHash": descriptionHash,
			"invoiceId":       invoiceId,
			"expiry":          expiry,
			"appId":           app.ID,
			"userId":          app.User.ID,
		}).Errorf("Failed to make invoice: %v", err)
		return nil, err
	}

	if resp.StatusCode < 300 {
		responsePayload := &StrikeMakeInvoiceResponse{}
		err = json.NewDecoder(resp.Body).Decode(responsePayload)
		if err != nil {
			return nil, err
		}
		svc.Logger.WithFields(logrus.Fields{
			"senderPubkey":    senderPubkey,
			"amount":          amount,
			"description":     description,
			"descriptionHash": descriptionHash,
			"expiry":          expiry,
			"appId":           app.ID,
			"userId":          app.User.ID,
			"paymentRequest":  responsePayload.LnInvoice,
			"invoiceId":       invoiceId,
			// "paymentHash":     "paymentHash",
		}).Info("Make invoice successful")
		// Payment hash is unsupported
		return &Nip47Transaction{
			Type:            "incoming",
			Invoice:         responsePayload.LnInvoice,
			Description:     description,
			DescriptionHash: descriptionHash,
			// Passing invoiceId as paymentHash for now
			PaymentHash: invoiceId,
			ExpiresAt:   &responsePayload.Expiry,
			Amount:      amount,
		}, nil
	}

	errorPayload := &StrikeErrorResponse{}
	err = json.NewDecoder(resp.Body).Decode(errorPayload)
	svc.Logger.WithFields(logrus.Fields{
		"senderPubkey":    senderPubkey,
		"amount":          amount,
		"description":     description,
		"descriptionHash": descriptionHash,
		"expiry":          expiry,
		"appId":           app.ID,
		"userId":          app.User.ID,
		"APIHttpStatus":   resp.StatusCode,
	}).Errorf("Make invoice failed %s", string(errorPayload.Data.Message))
	return nil, errors.New(errorPayload.Data.Message)
}

func (svc *StrikeOAuthService) GetBalance(ctx context.Context, senderPubkey string) (balance int64, err error) {
	app := App{}
	err = svc.db.Preload("User").First(&app, &App{
		NostrPubkey: senderPubkey,
	}).Error
	if err != nil {
		svc.Logger.WithFields(logrus.Fields{
			"senderPubkey": senderPubkey,
		}).Errorf("App not found: %v", err)
		return 0, err
	}
	tok, err := svc.FetchUserToken(ctx, app)
	if err != nil {
		return 0, err
	}
	client := svc.oauthConf.Client(ctx, tok)

	req, err := http.NewRequest("GET", fmt.Sprintf("%s/balances", svc.cfg.OAuthAPIURL), nil)
	if err != nil {
		svc.Logger.WithError(err).Error("Error creating request /balances")
		return 0, err
	}

	req.Header.Set("User-Agent", "NWC")

	resp, err := client.Do(req)
	if err != nil {
		svc.Logger.WithFields(logrus.Fields{
			"senderPubkey": senderPubkey,
			"appId":        app.ID,
			"userId":       app.User.ID,
		}).Errorf("Failed to fetch balance: %v", err)
		return 0, err
	}

	if resp.StatusCode < 300 {
		var responsePayload []StrikeBalanceResponse
		responseBody, _ := io.ReadAll(resp.Body)
		err = json.Unmarshal([]byte(responseBody), &responsePayload)
		if err != nil {
			return 0, err
		}
		// Will this always exist?
		for _, balanceResp := range responsePayload {
			if balanceResp.Currency == "BTC" {
				available, _ := strconv.ParseFloat(balanceResp.Available, 64)
				balance = int64(available * math.Pow10(8))
			}
		}
		svc.Logger.WithFields(logrus.Fields{
			"senderPubkey": senderPubkey,
			"appId":        app.ID,
			"userId":       app.User.ID,
		}).Info("Balance fetch successful")
		return balance, nil
	}

	errorPayload := &StrikeErrorResponse{}
	err = json.NewDecoder(resp.Body).Decode(errorPayload)
	svc.Logger.WithFields(logrus.Fields{
		"senderPubkey":  senderPubkey,
		"appId":         app.ID,
		"userId":        app.User.ID,
		"APIHttpStatus": resp.StatusCode,
	}).Errorf("Balance fetch failed %s", string(errorPayload.Data.Message))
	return 0, errors.New(errorPayload.Data.Message)
}

func (svc *StrikeOAuthService) SendPaymentSync(ctx context.Context, senderPubkey, payReq string) (preimage string, err error) {
	app := App{}
	err = svc.db.Preload("User").First(&app, &App{
		NostrPubkey: senderPubkey,
	}).Error
	if err != nil {
		svc.Logger.WithFields(logrus.Fields{
			"senderPubkey": senderPubkey,
			"bolt11":       payReq,
		}).Errorf("App not found: %v", err)
		return "", err
	}
	svc.Logger.WithFields(logrus.Fields{
		"senderPubkey": senderPubkey,
		"bolt11":       payReq,
		"appId":        app.ID,
		"userId":       app.User.ID,
	}).Info("Processing payment request")
	tok, err := svc.FetchUserToken(ctx, app)
	if err != nil {
		return "", err
	}
	client := svc.oauthConf.Client(ctx, tok)

	body := bytes.NewBuffer([]byte{})
	payload := &StrikePayRequest{
		LnInvoice:      payReq,
		SourceCurrency: "BTC",
	}
	err = json.NewEncoder(body).Encode(payload)

	req, err := http.NewRequest("POST", fmt.Sprintf("%s/payment-quotes/lightning", svc.cfg.OAuthAPIURL), body)
	if err != nil {
		svc.Logger.WithError(err).Error("Error creating request /payment-quotes/lightning")
		return "", err
	}

	req.Header.Set("User-Agent", "NWC")
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		svc.Logger.WithFields(logrus.Fields{
			"senderPubkey": senderPubkey,
			"bolt11":       payReq,
			"appId":        app.ID,
			"userId":       app.User.ID,
		}).Errorf("Failed to create quote: %v", err)
		return "", err
	}

	if resp.StatusCode >= 300 {
		errorPayload := &StrikeErrorResponse{}
		err = json.NewDecoder(resp.Body).Decode(errorPayload)
		svc.Logger.WithFields(logrus.Fields{
			"senderPubkey":  senderPubkey,
			"bolt11":        payReq,
			"appId":         app.ID,
			"userId":        app.User.ID,
			"APIHttpStatus": resp.StatusCode,
		}).Errorf("Payment failed %s", string(errorPayload.Data.Message))
		return "", errors.New(errorPayload.Data.Message)
	}

	responsePayload := &StrikePaymentQuoteResponse{}
	err = json.NewDecoder(resp.Body).Decode(responsePayload)
	if err != nil {
		return "", err
	}

	req, err = http.NewRequest("PATCH", fmt.Sprintf("%s/payment-quotes/%s/execute", svc.cfg.OAuthAPIURL, responsePayload.PaymentQuoteId), nil)
	if err != nil {
		svc.Logger.WithError(err).Errorf("Error creating request /payment-quotes/%s/execute", responsePayload.PaymentQuoteId)
		return "", err
	}

	req.Header.Set("User-Agent", "NWC")
	req.Header.Set("Content-Type", "application/json")

	resp, err = client.Do(req)
	if err != nil {
		svc.Logger.WithFields(logrus.Fields{
			"senderPubkey": senderPubkey,
			"bolt11":       payReq,
			"appId":        app.ID,
			"userId":       app.User.ID,
		}).Errorf("Failed to pay invoice: %v", err)
		return "", err
	}

	if resp.StatusCode < 300 {
		responsePayload := &StrikePaymentResponse{}
		err = json.NewDecoder(resp.Body).Decode(responsePayload)
		if err != nil {
			return "", err
		}
		svc.Logger.WithFields(logrus.Fields{
			"senderPubkey": senderPubkey,
			"bolt11":       payReq,
			"appId":        app.ID,
			"userId":       app.User.ID,
			"paymentId":    responsePayload.PaymentId,
		}).Info("Payment successful")
		// What to return here?
		return "preimage", nil
	}

	errorPayload := &StrikeErrorResponse{}
	err = json.NewDecoder(resp.Body).Decode(errorPayload)
	svc.Logger.WithFields(logrus.Fields{
		"senderPubkey":  senderPubkey,
		"bolt11":        payReq,
		"appId":         app.ID,
		"userId":        app.User.ID,
		"APIHttpStatus": resp.StatusCode,
	}).Errorf("Payment failed %s", string(errorPayload.Data.Message))
	return "", errors.New(errorPayload.Data.Message)
}

func (svc *StrikeOAuthService) AuthHandler(c echo.Context) error {
	appName := c.QueryParam("c") // c - for client
	// clear current session
	sess, _ := session.Get(CookieName, c)
	if sess.Values["user_id"] != nil {
		delete(sess.Values, "user_id")
		sess.Options.MaxAge = 0
		sess.Options.SameSite = http.SameSiteLaxMode
		if svc.cfg.CookieDomain != "" {
			sess.Options.Domain = svc.cfg.CookieDomain
		}
	}

	cv := oauth2.GenerateVerifier()

	sess.Values["code_verifier"] = cv
	sess.Save(c.Request(), c.Response())

	url := svc.oauthConf.AuthCodeURL(
		appName,
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		oauth2.SetAuthURLParam("code_challenge", oauth2.S256ChallengeFromVerifier(cv)),
	)

	return c.Redirect(302, url)
}

func (svc *StrikeOAuthService) CallbackHandler(c echo.Context) error {
	code := c.QueryParam("code")
	sess, _ := session.Get(CookieName, c)
	cv, ok := sess.Values["code_verifier"].(string)
	if !ok {
		err := errors.New("Code verifier not found in session")
		svc.Logger.WithError(err)
		return err
	}
	tok, err := svc.oauthConf.Exchange(
		c.Request().Context(),
		code,
		oauth2.SetAuthURLParam("code_verifier", cv),
		oauth2.SetAuthURLParam("redirect_uri", svc.cfg.OAuthRedirectUrl),
	)
	if err != nil {
		svc.Logger.WithError(err).Error("Failed to exchange token")
		return err
	}

	token, _, err := new(jwt.Parser).ParseUnverified(tok.AccessToken, jwt.MapClaims{})
	if err != nil {
		err := errors.New("Error parsing token")
		svc.Logger.WithError(err)
		return err
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		err := errors.New("Error parsing token claims")
		svc.Logger.WithError(err)
		return err
	}

	user := User{}
	// Should we change to NodeIdentifier?
	svc.db.FirstOrInit(&user, User{AlbyIdentifier: claims["sub"].(string)})
	user.AccessToken = tok.AccessToken
	user.RefreshToken = tok.RefreshToken
	user.Expiry = tok.Expiry // TODO; probably needs some calculation
	user.Email = claims["email"].(string)
	svc.db.Save(&user)

	sess.Options.MaxAge = 0
	sess.Options.SameSite = http.SameSiteLaxMode
	if svc.cfg.CookieDomain != "" {
		sess.Options.Domain = svc.cfg.CookieDomain
	}
	sess.Values["user_id"] = user.ID
	sess.Save(c.Request(), c.Response())
	return c.Redirect(302, "/")
}
