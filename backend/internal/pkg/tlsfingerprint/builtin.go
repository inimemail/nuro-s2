package tlsfingerprint

import utls "github.com/refraction-networking/utls"

var builtinStableProfiles = []Profile{
	{Name: "Codex Node.js 24 Default"},
	{Name: "Codex Node.js 24 GREASE", EnableGREASE: true},
	{Name: "uTLS Chrome 133", ClientHelloID: "HelloChrome_133"},
	{Name: "uTLS Chrome 131", ClientHelloID: "HelloChrome_131"},
	{Name: "uTLS Chrome 120", ClientHelloID: "HelloChrome_120"},
	{Name: "uTLS Chrome 120 PQ", ClientHelloID: "HelloChrome_120_PQ"},
	{Name: "uTLS Chrome 115 PQ", ClientHelloID: "HelloChrome_115_PQ"},
	{Name: "uTLS Chrome 106 Shuffle", ClientHelloID: "HelloChrome_106_Shuffle"},
	{Name: "uTLS Chrome 102", ClientHelloID: "HelloChrome_102"},
	{Name: "uTLS Chrome 100", ClientHelloID: "HelloChrome_100"},
	{Name: "uTLS Chrome 96", ClientHelloID: "HelloChrome_96"},
	{Name: "uTLS Chrome 87", ClientHelloID: "HelloChrome_87"},
	{Name: "uTLS Chrome 83", ClientHelloID: "HelloChrome_83"},
	{Name: "uTLS Firefox 120", ClientHelloID: "HelloFirefox_120"},
	{Name: "uTLS Firefox 105", ClientHelloID: "HelloFirefox_105"},
	{Name: "uTLS Firefox 102", ClientHelloID: "HelloFirefox_102"},
	{Name: "uTLS Firefox 99", ClientHelloID: "HelloFirefox_99"},
	{Name: "uTLS iOS 14", ClientHelloID: "HelloIOS_14"},
	{Name: "uTLS iOS 13", ClientHelloID: "HelloIOS_13"},
	{Name: "uTLS Safari 16.0", ClientHelloID: "HelloSafari_16_0"},
	{Name: "uTLS Android 11 OkHttp", ClientHelloID: "HelloAndroid_11_OkHttp"},
}

var clientHelloIDs = map[string]utls.ClientHelloID{
	"HelloChrome_Auto":        utls.HelloChrome_Auto,
	"HelloChrome_133":         utls.HelloChrome_133,
	"HelloChrome_131":         utls.HelloChrome_131,
	"HelloChrome_120":         utls.HelloChrome_120,
	"HelloChrome_120_PQ":      utls.HelloChrome_120_PQ,
	"HelloChrome_115_PQ":      utls.HelloChrome_115_PQ,
	"HelloChrome_106_Shuffle": utls.HelloChrome_106_Shuffle,
	"HelloChrome_102":         utls.HelloChrome_102,
	"HelloChrome_100":         utls.HelloChrome_100,
	"HelloChrome_96":          utls.HelloChrome_96,
	"HelloChrome_87":          utls.HelloChrome_87,
	"HelloChrome_83":          utls.HelloChrome_83,
	"HelloFirefox_Auto":       utls.HelloFirefox_Auto,
	"HelloFirefox_120":        utls.HelloFirefox_120,
	"HelloFirefox_105":        utls.HelloFirefox_105,
	"HelloFirefox_102":        utls.HelloFirefox_102,
	"HelloFirefox_99":         utls.HelloFirefox_99,
	"HelloIOS_Auto":           utls.HelloIOS_Auto,
	"HelloIOS_14":             utls.HelloIOS_14,
	"HelloIOS_13":             utls.HelloIOS_13,
	"HelloSafari_Auto":        utls.HelloSafari_Auto,
	"HelloSafari_16_0":        utls.HelloSafari_16_0,
	"HelloAndroid_11_OkHttp":  utls.HelloAndroid_11_OkHttp,
	"HelloEdge_Auto":          utls.HelloEdge_Auto,
	"HelloEdge_85":            utls.HelloEdge_85,
}

func BuiltinStableProfileCount() int {
	return len(builtinStableProfiles)
}

func BuiltinStableProfileForIndex(index int) *Profile {
	if len(builtinStableProfiles) == 0 {
		return nil
	}
	if index < 0 {
		index = -index
	}
	p := builtinStableProfiles[index%len(builtinStableProfiles)]
	return &p
}

func ResolveClientHelloID(name string) (utls.ClientHelloID, bool) {
	id, ok := clientHelloIDs[name]
	return id, ok
}
