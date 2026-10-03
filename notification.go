package main

// notification is a desktop notification about an image, which opens the
// image when clicked.
type notification struct {
	image    string // path to the image, also shown as a thumbnail
	summary  string
	body     string
	category string // freedesktop.org notification category, may be empty
}
