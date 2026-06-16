//go:build darwin

// Package secrets reads credentials from the macOS Keychain. Reading from this
// binary directly (rather than shelling out to /usr/bin/security) lets the
// Keychain item's ACL be bound to this program's own code identity, so only
// ebeco-spot — not any caller of the security tool — can read it unprompted.
package secrets

/*
#cgo LDFLAGS: -framework CoreFoundation -framework Security
#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>
#include <stdlib.h>
#include <string.h>

// ebeco_keychain_read looks up a generic-password item by service name and, on
// success, returns its data through *out/*outlen (malloc'd; caller frees) and
// returns 0. On failure it returns the non-zero OSStatus.
static int ebeco_keychain_read(const char *service, char **out, int *outlen) {
	CFStringRef svc = CFStringCreateWithCString(kCFAllocatorDefault, service, kCFStringEncodingUTF8);
	const void *keys[] = { kSecClass, kSecAttrService, kSecReturnData, kSecMatchLimit };
	const void *vals[] = { kSecClassGenericPassword, svc, kCFBooleanTrue, kSecMatchLimitOne };
	CFDictionaryRef query = CFDictionaryCreate(kCFAllocatorDefault, keys, vals, 4,
		&kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);

	CFTypeRef result = NULL;
	OSStatus status = SecItemCopyMatching(query, &result);
	CFRelease(query);
	CFRelease(svc);
	if (status != errSecSuccess) {
		return (int)status;
	}

	CFDataRef data = (CFDataRef)result;
	CFIndex n = CFDataGetLength(data);
	char *buf = malloc((size_t)n);
	memcpy(buf, CFDataGetBytePtr(data), (size_t)n);
	CFRelease(result);
	*out = buf;
	*outlen = (int)n;
	return 0;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

// Read returns the data of the generic-password Keychain item with the given
// service name. The first access by a freshly (re)built binary may prompt for
// authorization, since an unsigned binary is matched by its code hash.
func Read(service string) (string, error) {
	cService := C.CString(service)
	defer C.free(unsafe.Pointer(cService))

	var out *C.char
	var n C.int
	if status := C.ebeco_keychain_read(cService, &out, &n); status != 0 {
		return "", fmt.Errorf("keychain item %q not readable (OSStatus %d)", service, int(status))
	}
	defer C.free(unsafe.Pointer(out))
	return C.GoStringN(out, n), nil
}
