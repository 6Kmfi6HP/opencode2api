package app

import (
	"context"
	"encoding/hex"
	"net"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// bunReferenceClientHello 是 2026-10-24 抓包真实 opencode CLI v2.1.4
// （Servername=opencode.ai）发往 opencode.ai 的完整 ClientHello 原始字节。
// 见 docs/labs/2026-10-24-bun-fingerprint-alignment.md。
const bunReferenceClientHello = "16030105d9010005d50303a3c230b306e5b47d0ea0bf3b732383f86e4481892dda910c6b911d573ba6e60b20629b615ec7fa9a90460021b0523776f5fc971cd6f045cbb1876ddc7c3fd011550022130113021303c02bc02fc02cc030cca9cca8c009c013c00ac014009c009d002f00350100056a00000010000e00000b6f70656e636f64652e616900170000ff01000100000a000a000811ec001d00170018000b00020100002300000010000b000908687474702f312e31000500050100000000000d001800160403080404010503080505010806060108070603020100120000003304ea04e811ec04c094c492054a8e8f557eb2fb12943732b1b12cba876401708ac7756fd49582d1a190ef8a959c26b8f8f37ece7048afb09bbb5b3890a3021268649261817d96c71052c9d84b3c9873c91ba075c0b3265e500272d3008bfc5413a320f9d2ba0074293666a376fb02d3c3407f62c464f78cb44214706950a1b47e6eb44465c516c3d9c0df391f7e6c95d3a87ac0f585e3d44fca412d42805b3c720e1972aeaadab43db2b0eec3c53a6c035da00709e53ef9e8bc68d41b6a81b9ee40870d136740f37d6a27ad76801bda947e00e119f11cab6948463a864174177de525641b0882fd951dfe99a93730cbd890431fc875d7b2a6f30a7737aa07c3b4476a1c22a85a11948350a964878509770b051e5891b2bf65629b8394d8407f5fb8ada65295b3474099d01ef00b4c3fbcafd6a9ce41b3784d202c9e5cb608407c844399c95047ee692e57231226da1ad58a97ce27cbe8674887e9a6cb68a4b32623e3b8325873a570b9bbee197b2019ca7c2c0edc76a42956c941c72385db925729831ac37ce16c0814722e0fb4c0821a7b790a5429588238529f9ba09d7a22c58ac59baa62a955b4c5aec2365a8ab9b5ab01c2f48a1557b1f2da2e6db17c5020338299591ea93a63a2938962066192343c0892ffa316e838ce5eca3c41f9c57b1580083c677a91a9dde385ce2002f074ce329b4b961416f0872ad6932fa7548245ab53920b646a289e5772bcd80a3bc2c7871dd4bff2d1a6bab1090dac39fc1931d5d42679578854531f9eba3cee6a49b80203efc3260fe7b5cb043d4b0a76a54c4768a999b6a28b04d80ca9b84b0218aa7e366f8cc53f4ccb8b6f6797448b8596634383c1a2b2886496110049ca6c68527583cb7aa6f1037bb0979091ba98c7c9de7192b2b85ba39a895727ce201b18000041dcd760bbe75c6c61ce254620e698acd3c8334b45c7ac9389c1f0ce1057489b48783301064530c8bef80546660504814899978dd66937582462a9207c912b70612796740b83b8ac0e1a2b51c6b511c2a2a9d1db894a23052619a557a1796e97ba988993e0e4aad9b2a9dfe3b95a9a0c8ecc89e24c8a13bacc12052bb5b340274b200d992e4fd353bb1376b8fccc61e82c77d747253a29d5e614e5722900da6e9348a410d34915db1129e6920d3154268a56ac62942e2a2dd2cb93d76648a4519b76db486fc9bf6376320fc140191319181a264b761c79d5b141a1561862caa94181792b664fb601053c53329b21e80a0a9e465d1aec11a6590537a193cd60bb7d6aaf94e5a107f166a26b1624cc2f2af38c44937ef7454bdf6521236b0d42428fa1fb0bd1baa67798099b59caf76957a6428e44916597159d5efb5cb6e5aa738b7d91812cd8c3b68b526a0817533f36cb6f72652e5a7183257dd4c5c9eb34508ce653c0b0a39e2269a8093b7d7a1220c472f5732099cb234ab3485d7a8a686b68561b8544791b94412c2b88c8ebe2362c2026b0a50ddbf58687117221b69139b8588ee4210232187514bafe848d4193021caaa9d8b64d7f0614747a13e3b2879c117cda1240a9744bfe10bf3967a2cc1985ccb22b9207353fc12a04154b12141b2e802a6cc78ae40a05a9f78fcde9302d60b62c74335f36a5e448539598cd2c6d1067c34c737e318343628c49c0ca348f3ff9025f8a6d1b7d0ed94c1832ad90a2797885a4f5ad1a067d4454c40c83862ec783672a68001d0020677a8a47b3b9f5ce588fb7d002a0f517e887de9845f334e33a250d7b61064145002d00020101002b00050403040303"

// captureClientHello 用 net.Pipe 把 bun 档 spec 生成的 ClientHello 抓下来。
func captureClientHello(t *testing.T, sni string) []byte {
	t.Helper()
	c1, c2 := net.Pipe()
	defer c2.Close()

	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4096)
		var got []byte
		for {
			c2.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, err := c2.Read(buf)
			if n > 0 {
				got = append(got, buf[:n]...)
			}
			if err != nil || len(got) >= 5 {
				// 已拿到完整记录头，按长度截断
				if len(got) >= 5 {
					need := 5 + int(got[3])<<8 + int(got[4])
					if len(got) >= need {
						done <- got[:need]
						return
					}
					continue
				}
				done <- got
				return
			}
		}
	}()

	u := utls.UClient(c1, &utls.Config{ServerName: sni}, utls.HelloCustom)
	if err := u.ApplyPreset(bunClientHelloSpec(nil)); err != nil {
		t.Fatalf("ApplyPreset: %v", err)
	}
	if net.ParseIP(sni) != nil {
		if err := u.RemoveSNIExtension(); err != nil {
			t.Fatalf("RemoveSNIExtension: %v", err)
		}
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = u.HandshakeContext(ctx) // 对端不会回 ServerHello，必然失败，hello 已写出
	}()

	select {
	case b := <-done:
		return b
	case <-time.After(3 * time.Second):
		t.Fatal("timed out capturing ClientHello")
		return nil
	}
}

// normalizeClientHello 抹掉每次连接都会变的随机字段（random / session id /
// key_share 公钥），只保留结构指纹。
func normalizeClientHello(t *testing.T, b []byte) []byte {
	t.Helper()
	out := append([]byte(nil), b...)
	body := out[5:]
	if len(body) < 44 || body[0] != 0x01 {
		t.Fatalf("not a ClientHello")
	}
	body = body[4:] // 跳过 handshake type + 3 字节长度
	// random
	copy(body[2:34], make([]byte, 32))
	// session id
	sidLen := int(body[34])
	off := 35
	copy(body[off:off+sidLen], make([]byte, sidLen))
	off += sidLen
	// ciphers
	csLen := int(body[off])<<8 | int(body[off+1])
	off += 2 + csLen
	// compression
	compLen := int(body[off])
	off += 1 + compLen
	// extensions
	extLen := int(body[off])<<8 | int(body[off+1])
	off += 2
	end := off + extLen
	for off+4 <= end {
		et := int(body[off])<<8 | int(body[off+1])
		el := int(body[off+2])<<8 | int(body[off+3])
		val := body[off+4 : off+4+el]
		if et == 51 && len(val) >= 4 { // key_share: 抹掉每个条目的公钥
			inner := 2
			for inner+4 <= len(val) {
				l := int(val[inner+2])<<8 | int(val[inner+3])
				copy(val[inner+4:inner+4+l], make([]byte, l))
				inner += 4 + l
			}
		}
		off += 4 + el
	}
	return out
}

// TestBunClientHelloSpec_MatchesCapture 断言 bun 档生成的 ClientHello 与真实
// opencode CLI 抓包逐字节一致（随机字段归一化后）。
func TestBunClientHelloSpec_MatchesCapture(t *testing.T) {
	want, err := hex.DecodeString(bunReferenceClientHello)
	if err != nil {
		t.Fatal(err)
	}
	got := captureClientHello(t, "opencode.ai")

	gotN := normalizeClientHello(t, got)
	wantN := normalizeClientHello(t, want)

	if len(gotN) != len(wantN) {
		t.Fatalf("length mismatch: got %d want %d", len(gotN), len(wantN))
	}
	for i := range gotN {
		if gotN[i] != wantN[i] {
			t.Fatalf("differs at byte %d:\ngot  %s\nwant %s", i,
				hex.EncodeToString(gotN[i:min(i+16, len(gotN))]),
				hex.EncodeToString(wantN[i:min(i+16, len(wantN))]))
		}
	}
}

// TestBunClientHelloSpec_SNI 域名连接时 server_name 出现在首位且取值为域名。
func TestBunClientHelloSpec_SNI(t *testing.T) {
	raw := captureClientHello(t, "example.com")
	if hexOf := hex.EncodeToString(raw); len(hexOf) == 0 {
		t.Fatal("empty")
	}
	// 找到 extensions 段，断言第一个扩展是 server_name(0) 且包含域名。
	body := raw[9:] // 跳过 record header(5) + handshake type/len(4)
	off := 35 + int(body[34])
	off += 2 + (int(body[off])<<8 | int(body[off+1]))
	off += 1 + int(body[off])
	off += 2 // ext total len
	if int(body[off])<<8|int(body[off+1]) != 0 {
		t.Fatalf("first extension is not server_name: %x", body[off:off+2])
	}
	rest := body[off:]
	if !containsBytes(rest, []byte("example.com")) {
		t.Fatalf("SNI hostname missing from ClientHello")
	}
}

func containsBytes(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
