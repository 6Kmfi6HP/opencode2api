package app

import (
	utls "github.com/refraction-networking/utls"
)

// ======================== Bun / BoringSSL ClientHello 档位 ========================
//
// 2026-10-24 抓包真实 opencode CLI v2.1.4（Bun 编译产物）打到 opencode.ai 的
// ClientHello 逐字节复刻：17 个 cipher（BoringSSL 默认档，非 Chrome 档）、
// 13 个扩展、固定顺序、无 GREASE / ALPS，ALPN 只 offer http/1.1。
//
// 抓包与差分方法见 docs/labs/2026-10-24-bun-fingerprint-alignment.md；与
// Chrome 档（utls.HelloChrome_Auto = HelloChrome_133）的关键差异：
//   - CLI 有 MLKEM 混合密钥共享（X25519MLKEM768/0x11ec，1216B）且排首位
//   - CLI 带 status_request(OCSP) 与 SCT，无 ALPS
//   - CLI 的 ALPN 只有 http/1.1 —— 真实客户端全程 HTTP/1.1，不走 h2
//   - Chrome 扩展顺序每次连接随机洗牌，BoringSSL/CLI 固定
//
// 真实指纹样本（JA3）：771,4865-4866-4867-49195-49199-49196-49200-52393-52392-
// 49161-49171-49162-49172-156-157-47-53,
// 0-23-65281-10-11-35-16-5-13-18-51-45-43,4588-29-23-24,0

// bunFingerprintALPN 是真实 CLI 抓包中实际发出的 ALPN 列表：只有 http/1.1。
// 上游全程 HTTP/1.1，offer h2 本身就是与真实客户端不一致的特征。
var bunFingerprintALPN = []string{"http/1.1"}

// bunClientHelloSpec 返回 Bun/BoringSSL 档的 ClientHelloSpec。
// 扩展顺序即绘制到线上的顺序；uTLS 不会重排，也不会追加 GREASE。
func bunClientHelloSpec(alpn []string) *utls.ClientHelloSpec {
	if len(alpn) == 0 {
		alpn = bunFingerprintALPN
	}
	return &utls.ClientHelloSpec{
		CipherSuites: []uint16{
			// TLS 1.3
			0x1301, 0x1302, 0x1303,
			// ECDHE-ECDSA/RSA GCM
			0xc02b, 0xc02f, 0xc02c, 0xc030,
			// ECDHE-ECDSA/RSA CHACHA20
			0xcca9, 0xcca8,
			// ECDHE-ECDSA/RSA CBC
			0xc009, 0xc013, 0xc00a, 0xc014,
			// RSA GCM / RSA CBC
			0x009c, 0x009d, 0x002f, 0x0035,
		},
		CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			// SNIExtension 在 spec 中占首位（BoringSSL 的顺序）；IP 直连时由
			// 调用方 RemoveSNIExtension() 去掉，与 BoringSSL 不发空 SNI 一致。
			&utls.SNIExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.X25519MLKEM768, utls.CurveX25519, utls.CurveSECP256R1, utls.CurveP384,
			}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: alpn},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				utls.ECDSAWithP256AndSHA256, // 0x0403
				utls.PSSWithSHA256,          // 0x0804
				utls.PKCS1WithSHA256,        // 0x0401
				utls.ECDSAWithP384AndSHA384, // 0x0503
				utls.PSSWithSHA384,          // 0x0805
				utls.PKCS1WithSHA384,        // 0x0501
				utls.PSSWithSHA512,          // 0x0806
				utls.PKCS1WithSHA512,        // 0x0601
				utls.Ed25519,                // 0x0807
				utls.ECDSAWithP521AndSHA512, // 0x0603
				utls.PKCS1WithSHA1,          // 0x0201
			}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
				{Group: utls.X25519MLKEM768},
				{Group: utls.CurveX25519},
			}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			&utls.SupportedVersionsExtension{Versions: []uint16{
				utls.VersionTLS13, utls.VersionTLS12,
			}},
			// BoringSSL 的 F5 规避：未填充长度落在 [256,511] 时用 padding 补到
			// 512 字节（实测 Bun 带域名时 hello 恰为 512 字节）。uTLS 内置的
			// BoringPaddingStyle 就是 BoringSSL 的实现，直接用即可。
			&utls.UtlsPaddingExtension{GetPaddingLen: utls.BoringPaddingStyle},
		},
		TLSVersMin: utls.VersionTLS12,
		TLSVersMax: utls.VersionTLS13,
	}
}
