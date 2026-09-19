locals {
  a = <<EOT0
resource "google_kms_crypto_key" "hidden0" {
EOT0
  b = <<EOT1
resource "google_kms_crypto_key" "hidden1" {
EOT1
  c = <<EOT2
resource "google_kms_crypto_key" "hidden2" {
EOT2
  d = <<EOT3
resource "google_kms_crypto_key" "hidden3" {
 EOT3
  e = <<EOT4
resource "google_kms_crypto_key" "hidden4" {
 EOT4
}
resource "google_kms_crypto_key" "after" {
  name = "after"
}
