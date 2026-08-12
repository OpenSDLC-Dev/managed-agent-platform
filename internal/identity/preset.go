package identity

// gcpIAPPreset is Mode B's one shipped preset, as data.
//
// Every value is a literal pinned by a test, so an edit to the header name or the
// key URL fails a test rather than a production deployment.
//
// The audience is deliberately not here: it is per-deployment, required, and it
// is the entire tenant boundary. https://www.gstatic.com/iap/verify/public_key-jwk
// is Google's global key set — every Google Cloud customer's IAP assertion is
// signed by a key this deployment would trust — so an empty or wrong audience is
// not a misconfiguration, it is cross-customer authentication.
//
// The legacy https://www.gstatic.com/iap/verify/public_key endpoint (a JSON
// object of kid → PEM certificate) is deliberately unsupported: a second
// key-source shape for one preset would need its own decoder inside the security
// core, and it cannot express the kty/crv/use validation the JWK Set path gets
// for free.
var gcpIAPPreset = struct {
	Header, Issuer, KeysURL string
	Algorithms              []string
}{
	Header:     "x-goog-iap-jwt-assertion",
	Issuer:     "https://cloud.google.com/iap",
	KeysURL:    "https://www.gstatic.com/iap/verify/public_key-jwk",
	Algorithms: []string{"ES256"},
}
