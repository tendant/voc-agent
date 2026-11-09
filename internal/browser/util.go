package browser

import "net/url"

func urlQuery(s string) string { return url.QueryEscape(s) }
