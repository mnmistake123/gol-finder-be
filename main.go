package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"io/ioutil"
	"net/http"
	"os"
	"time"
	"github.com/stripe/stripe-go/v75"
	"github.com/stripe/stripe-go/v75/customer"
	"github.com/stripe/stripe-go/v75/ephemeralkey"
	"github.com/stripe/stripe-go/v75/paymentintent"
	"github.com/stripe/stripe-go/v75/webhook"

	resend "github.com/resend/resend-go/v3"
	"cloud.google.com/go/firestore"
	firebase "firebase.google.com/go/v4"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var firestoreClient *firestore.Client

func main() {
	ctx := context.Background()

	var err error
	firestoreClient, err = initFirestore(ctx)
	if err != nil {
		log.Fatalf("Failed to initialize Firestore: %v", err)
	}
	defer firestoreClient.Close()

	stripe.Key = os.Getenv("STRIPE_SECRET_KEY")

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Server is running")
	})

	http.HandleFunc("/payment-sheet", handlePaymentSheet)
	http.HandleFunc("/comfirmation-web-hook", handleStripeWebhook)
	http.HandleFunc("/remove-from-match", handleRemoveFromMatch)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	fmt.Println("Server running on port", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func handlePaymentSheet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Email     string  `json:"email"`
		UserId    string  `json:"userId"`
		MatchId    string `json:"matchId"`
		Name      string  `json:"name"`
		MatchDate string  `json:"matchDate"`
		Location  string  `json:"location"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	cparams := &stripe.CustomerParams{
		Email: stripe.String(body.Email),
		Name:  stripe.String(body.Name),
	}

	c, err := customer.New(cparams)
	if err != nil {
		http.Error(w, "Failed to create customer", http.StatusInternalServerError)
		return
	}

	ekparams := &stripe.EphemeralKeyParams{
		Customer:      stripe.String(c.ID),
		StripeVersion: stripe.String("2023-08-16"),
	}

	ek, err := ephemeralkey.New(ekparams)
	if err != nil {
		http.Error(w, "Failed to create ephemeral key", http.StatusInternalServerError)
		return
	}

	piparams := &stripe.PaymentIntentParams{
		Amount:   stripe.Int64(103),
		Currency: stripe.String(string(stripe.CurrencyUSD)),
		Customer: stripe.String(c.ID),
		AutomaticPaymentMethods: &stripe.PaymentIntentAutomaticPaymentMethodsParams{
			Enabled: stripe.Bool(true),
		},
		Metadata: map[string]string{
			"email": body.Email,
			"userId": body.UserId,
			"matchId": body.MatchId,
			"name": body.Name,
			"matchDate": body.MatchDate,
			"location": body.Location,
		},
	}
	
	pi, err := paymentintent.New(piparams)
	if err != nil {
		http.Error(w, "Failed to create payment intent", http.StatusInternalServerError)
		return
	}

	resp := struct {
		PaymentIntent  string `json:"paymentIntent"`
		EphemeralKey   string `json:"ephemeralKey"`
		Customer       string `json:"customer"`
		PublishableKey string `json:"publishableKey"`
	}{
		PaymentIntent:  pi.ClientSecret,
		EphemeralKey:   ek.Secret,
		Customer:       c.ID,
		PublishableKey: os.Getenv("STRIPE_PUBLISHABLE_KEY"),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	const MaxBodyBytes = int64(65536)
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	payload, err := ioutil.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Read error", http.StatusServiceUnavailable)
		return
	}

	event, err := webhook.ConstructEvent(payload, r.Header.Get("Stripe-Signature"), os.Getenv("STRIPE_WEBHOOK_SECRET"))
	if err != nil {
		log.Printf("Webhook error: %v", err)
		http.Error(w, "Signature error", http.StatusBadRequest)
		return
	}

	if event.Type == "payment_intent.succeeded" {
		var paymentIntent stripe.PaymentIntent
		if err := json.Unmarshal(event.Data.Raw, &paymentIntent); err != nil {
			http.Error(w, "Error parsing payment intent", http.StatusBadRequest)
			return
		}

		customerEmail := paymentIntent.Metadata["email"]
		userId := paymentIntent.Metadata["userId"]
		matchId := paymentIntent.Metadata["matchId"]
		customerName := paymentIntent.Metadata["name"]
		matchDate := paymentIntent.Metadata["matchDate"]
		matchLocation := paymentIntent.Metadata["location"]

		timestamp := time.Unix(paymentIntent.Created, 0)

		ctx := context.Background()
	
		err = saveOrderToFirestore(ctx, userId, matchId, paymentIntent.Amount, timestamp, paymentIntent.ID)
		if err != nil {
			fmt.Println("Error saving order to Firestore:", err)
			http.Error(w, "Database error", http.StatusInternalServerError)
			return
		}

		err = sendConfirmationEmail(customerName, customerEmail, matchDate, matchLocation)
		if err != nil {
			fmt.Println("Error sending email:", err)
			http.Error(w, "Email error", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

func handleRemoveFromMatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		MatchId   string `json:"matchId"`
		UserId    string `json:"userId"`
		IntentId  string `json:"intentId"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	log.Printf("Body received: %+v", body)

	ctx := context.Background()

	pi, err := paymentintent.Get(body.IntentId, nil)
	if err != nil {
		http.Error(w, "Payment intent not found", http.StatusNotFound)
		return
	}

	if pi.Status != stripe.PaymentIntentStatusSucceeded {
		http.Error(w, "Payment was not successful", http.StatusBadRequest)
		return
	}

	paymentRecordRef := firestoreClient.Collection("paymentRecords").Doc(body.IntentId)
	paymentRecordDoc, err := paymentRecordRef.Get(ctx)
	if err != nil {
		http.Error(w, "Payment record not found", http.StatusNotFound)
		return
	}

	paymentData := paymentRecordDoc.Data()
	if paymentData["userId"] != body.UserId || paymentData["matchId"] != body.MatchId || paymentData["paymentIntentID"] != body.IntentId {
		http.Error(w, "Payment record does not match provided credentials", http.StatusBadRequest)
		return
	}

	groundDoc, err := firestoreClient.Collection("Grounds").Doc(body.MatchId).Get(ctx)
	if err != nil {
		log.Printf("Match not found %v", err)
		http.Error(w, "Match not found", http.StatusNotFound)
		return
	}

	matchData := groundDoc.Data()
	gameTimestamp, ok := matchData["GameTimestamp"].(time.Time)
	if !ok {
		log.Printf("Invalid match data timestamp %v", gameTimestamp)
		http.Error(w, "Invalid match data timestamp", http.StatusInternalServerError)
		return
	}

	now := time.Now()
	oneDayBefore := gameTimestamp.Add(-24 * time.Hour)
	if now.After(oneDayBefore) {
		log.Printf("24 hours thing %v", oneDayBefore)
		
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Cancellation must be done at least 24 hours before the game",
		})
		return
	}

	customerEmail := pi.Metadata["email"]
	customerName := pi.Metadata["name"]
	matchDate := pi.Metadata["matchDate"]
	matchLocation := pi.Metadata["location"]

	if customerEmail == "" {
		http.Error(w, "Email not found in payment intent", http.StatusBadRequest)
		return
	}

	err = firestoreClient.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		balanceRef := firestoreClient.Collection("UserBalance").Doc(body.UserId)
		balanceDoc, err := tx.Get(balanceRef)
		if err != nil && status.Code(err) != codes.NotFound {
			return err
		}

		var currentBalance int64 = 0
		if err == nil {
			currentBalance = balanceDoc.Data()["balance"].(int64)
		}

		newBalance := currentBalance + pi.Amount

		if err := tx.Update(paymentRecordRef, []firestore.Update{
			{Path: "isCanceled", Value: true},
		}); err != nil {
			return err
		}

		if err := tx.Set(balanceRef, map[string]interface{}{
			"userId":  body.UserId,
			"balance": newBalance,
		}, firestore.MergeAll); err != nil {
			return err
		}

		if err := sendCancellationEmail(customerName, customerEmail, matchDate, matchLocation); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		fmt.Println("Error in transaction:", err)
		http.Error(w, "Failed to process cancellation", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"success": true,
		"message": "Successfully removed from match",
		"balance": fmt.Sprintf("%d", pi.Amount),
	})
}

func initFirestore(ctx context.Context) (*firestore.Client, error) {
    credsJSON := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS_JSON")
    sa := option.WithCredentialsJSON([]byte(credsJSON))

    conf := &firebase.Config{
        ProjectID: os.Getenv("FIREBASE_PROJECT_ID"),
    }

    app, err := firebase.NewApp(ctx, conf, sa)
    if err != nil {
        return nil, err
    }
    return app.Firestore(ctx)
}

func saveOrderToFirestore(ctx context.Context, userId string, matchId string, amount int64, timestamp time.Time, paymentIntentID string) error {
	order := map[string]interface{}{
		"userId":            userId,
		"matchId":           matchId,
		"amount":            amount,
		"timestamp":         timestamp,
		"paymentIntentID":   paymentIntentID,
		"isComfirmed":       true,
		"isCanceled":        false,
	}

	_, err := firestoreClient.Collection("paymentRecords").Doc(paymentIntentID).Set(ctx, order)
	return err
}

func sendConfirmationEmail(name string, to string, date string, location string) error {
	apiKey := os.Getenv("RESEND_API_KEY")
	client := resend.NewClient(apiKey)

	params := &resend.SendEmailRequest{
		From:    "Acme <onboarding@resend.dev>",
		To:      []string{to},
		Subject: "🎉 Tu partido ha sido confirmado",
		Html: fmt.Sprintf(`
			<h2>¡Hola %s!</h2>
			<h2>¡Gracias por tu pago!</h2>
			<p>Tu partido en <strong>GolFinder</strong> ha sido confirmado.</p>
			<p><b>Fecha:</b> %s<br/>
			<b>Lugar:</b> %s</p>
			<p>¡Nos vemos en la cancha! ⚽</p>
		`, name, date, location),
	}

	email, err := client.Emails.Send(params)
	if err != nil {
		return err
	}

	fmt.Println("Email sent! ID:", email.Id)
	return nil
}

func sendCancellationEmail(name string, to string, date string, location string) error {
	apiKey := os.Getenv("RESEND_API_KEY")
	client := resend.NewClient(apiKey)

	params := &resend.SendEmailRequest{
		From:    "Acme <onboarding@resend.dev>",
		To:      []string{to},
		Subject: "❌ Tu cancelación ha sido procesada",
		Html: fmt.Sprintf(`
			<h2>¡Hola %s!</h2>
			<h2>Tu cancelación ha sido procesada</h2>
			<p>Hemos cancelado tu participación en el partido de <strong>GolFinder</strong>.</p>
			<p><b>Fecha:</b> %s<br/>
			<b>Lugar:</b> %s</p>
			<p>Te hemos reembolsado el monto correspondiente a tu saldo.</p>
			<p>¡Esperamos verte en un próximo partido! ⚽</p>
		`, name, date, location),
	}

	email, err := client.Emails.Send(params)
	if err != nil {
		return err
	}

	fmt.Println("Cancellation email sent! ID:", email.Id)
	return nil
}
