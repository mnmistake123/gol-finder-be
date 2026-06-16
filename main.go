package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"io/ioutil"
	"net/http"
	"os"

	"github.com/stripe/stripe-go/v76"
	"github.com/stripe/stripe-go/v76/customer"
	"github.com/stripe/stripe-go/v76/ephemeralkey"
	"github.com/stripe/stripe-go/v76/paymentintent"
	"github.com/stripe/stripe-go/v76/webhook"

	resend "github.com/resend/resend-go/v2"
)

func main() {
	stripe.Key = os.Getenv("STRIPE_SECRET_KEY")

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Server is running")
	})

	http.HandleFunc("/payment-sheet", handlePaymentSheet)
	http.HandleFunc("/comfirmation-web-hook", handleStripeWebhook)

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

	cparams := &stripe.CustomerParams{}
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
		http.Error(w, "Signature error", http.StatusBadRequest)
		return
	}

	if event.Type == "checkout.session.completed" {
		var session stripe.CheckoutSession
		err := json.Unmarshal(event.Data.Raw, &session)
		if err != nil {
			http.Error(w, "Unmarshal error", http.StatusBadRequest)
			return
		}

		customerEmail := session.CustomerDetails.Email
		matchDate := session.Metadata["matchDate"]
		matchLocation := session.Metadata["location"]

		err = sendConfirmationEmail(customerEmail, matchDate, matchLocation)
		if err != nil {
			fmt.Println("Error sending email:", err)
			http.Error(w, "Email error", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}


func sendConfirmationEmail(to string, date string, location string) error {
	apiKey := os.Getenv("RESEND_API_KEY") // store this securely, e.g., in .env or secrets manager

	client := resend.NewClient(apiKey)

	params := &resend.SendEmailRequest{
		From:    "GolFinder <noreply@golfinder.com>", // must be a verified domain or sender in Resend
		To:      []string{"customer@example.com"},
		Subject: "🎉 Tu partido ha sido confirmado",
		Html: `
			<h2>¡Gracias por tu pago!</h2>
			<p>Tu partido en <strong>GolFinder</strong> ha sido confirmado.</p>
			<p><b>Fecha:</b> 10 de julio, 18:00 hrs<br/>
			<b>Lugar:</b> Parque La Carolina, Quito</p>
			<p>¡Nos vemos en la cancha! ⚽</p>
		`,
	}

	// You can also use Text: "text-only fallback" if needed

	email, err := client.Emails.Send(context.Background(), params)
	if err != nil {
		log.Fatalf("Error sending email: %v", err)
	}

	fmt.Println("Email sent! ID:", email.Id)
}
