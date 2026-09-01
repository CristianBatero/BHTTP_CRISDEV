//go:build android

package main

/*
#include <jni.h>
#include <stdlib.h>

static const char* bilolaGetStringUTF(JNIEnv* env, jstring value) {
	if (value == NULL) return NULL;
	return (*env)->GetStringUTFChars(env, value, NULL);
}

static void bilolaReleaseStringUTF(JNIEnv* env, jstring value, const char* text) {
	if (value != NULL && text != NULL) (*env)->ReleaseStringUTFChars(env, value, text);
}

static jstring bilolaNewStringUTF(JNIEnv* env, const char* value) {
	return (*env)->NewStringUTF(env, value == NULL ? "" : value);
}

static JavaVM* bilolaVM = NULL;

// Captures the JavaVM from any JNIEnv. Called from nativeCreate so we avoid
// defining JNI_OnLoad (Go's runtime/cgo already provides one for GOOS=android).
static int bilolaCaptureVM(JNIEnv* env) {
	if (bilolaVM != NULL) return 1;
	if (env == NULL) return 0;
	JavaVM* vm = NULL;
	if ((*env)->GetJavaVM(env, &vm) != JNI_OK || vm == NULL) return 0;
	bilolaVM = vm;
	return 1;
}

static JNIEnv* bilolaGetAttachedEnv(int* attached) {
	JNIEnv* env = NULL;
	if (bilolaVM == NULL) return NULL;
	jint rc = (*bilolaVM)->GetEnv(bilolaVM, (void**)&env, JNI_VERSION_1_6);
	if (rc == JNI_OK) {
		*attached = 0;
		return env;
	}
	if ((*bilolaVM)->AttachCurrentThread(bilolaVM, &env, NULL) != JNI_OK) {
		return NULL;
	}
	*attached = 1;
	return env;
}

static void bilolaDetachIfAttached(int attached) {
	if (attached) (*bilolaVM)->DetachCurrentThread(bilolaVM);
}

static int bilolaCallProtect(JNIEnv* env, jclass clazz, jmethodID mid, int fd) {
	if (env == NULL || clazz == NULL || mid == NULL) return 0;
	return (int)(*env)->CallStaticBooleanMethod(env, clazz, mid, (jint)fd);
}

static jclass bilolaNewGlobalRef(JNIEnv* env, jclass clazz) {
	return (jclass)(*env)->NewGlobalRef(env, clazz);
}

static jmethodID bilolaGetStaticProtectID(JNIEnv* env, jclass clazz) {
	return (*env)->GetStaticMethodID(env, clazz, "protectSocket", "(I)Z");
}
*/
import "C"

import (
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"bilola/mobile/bridge"
	"bilola/mobile/xhttpbridge"
)

var (
	protectMu sync.Mutex
)

// buildProtect returns a callback that asks the Android VpnService to protect
// the given socket file descriptor (exclude its packets from the tunnel).
// It must run before any bytes are sent on the socket, otherwise the outbound
// packet enters the VPN tunnel and causes a routing loop that destabilises the
// session. The class reference and method ID are captured once from the JNI
// call site so lookups stay free of classloader issues on worker threads.
// Returns nil when protection is unavailable (non-Android build or no VPN).
func buildProtect(env *C.JNIEnv, clazz C.jclass) func(int) bool {
	if env == nil || clazz == nil {
		return nil
	}
	if C.bilolaCaptureVM(env) != 1 {
		return nil
	}
	global := C.bilolaNewGlobalRef(env, clazz)
	mid := C.bilolaGetStaticProtectID(env, clazz)
	if global == nil || mid == nil {
		return nil
	}
	return func(fd int) bool {
		protectMu.Lock()
		defer protectMu.Unlock()
		var attached C.int
		e := C.bilolaGetAttachedEnv(&attached)
		if e == nil {
			return false
		}
		defer C.bilolaDetachIfAttached(attached)
		return C.bilolaCallProtect(e, global, mid, C.int(fd)) != 0
	}
}

type tunnelEngine interface {
	Start() (int, error)
	Close()
	Wait()
	UploadedBytes() uint64
	DownloadedBytes() uint64
	DrainLogs() string
	LastError() string
}

var (
	enginesMu sync.RWMutex
	engines   = make(map[int64]tunnelEngine)
	nextID    atomic.Int64
)

func engineByID(id int64) tunnelEngine {
	enginesMu.RLock()
	engine := engines[id]
	enginesMu.RUnlock()
	return engine
}

func storeEngine(engine tunnelEngine) C.jlong {
	id := nextID.Add(1)
	enginesMu.Lock()
	engines[id] = engine
	enginesMu.Unlock()
	return C.jlong(id)
}

func javaString(env *C.JNIEnv, value string) C.jstring {
	text := C.CString(value)
	defer C.free(unsafe.Pointer(text))
	return C.bilolaNewStringUTF(env, text)
}

//export Java_com_btun_client_GoBhttpBridge_nativeCreate
func Java_com_btun_client_GoBhttpBridge_nativeCreate(
	env *C.JNIEnv,
	clazz C.jclass,
	hostValue C.jstring,
	port C.jint,
	uploadSlots C.jint,
	downloadSlots C.jint,
) C.jlong {
	hostText := C.bilolaGetStringUTF(env, hostValue)
	if hostText == nil {
		return 0
	}
	host := C.GoString(hostText)
	C.bilolaReleaseStringUTF(env, hostValue, hostText)
	engine, err := bridge.New(bridge.Config{
		Host:                host,
		Port:                int(port),
		UploadConnections:   int(uploadSlots),
		DownloadConnections: int(downloadSlots),
		ConnectTimeout:      8 * time.Second,
		ReadTimeout:         15 * time.Second,
		ProtectSocket:       buildProtect(env, clazz),
	})
	if err != nil {
		return 0
	}
	return storeEngine(engine)
}

//export Java_com_btun_client_GoXhttpBridge_nativeCreate
func Java_com_btun_client_GoXhttpBridge_nativeCreate(
	env *C.JNIEnv,
	clazz C.jclass,
	proxyHostValue C.jstring,
	proxyPort C.jint,
	serverNameValue C.jstring,
	hostHeaderValue C.jstring,
	insecure C.jboolean,
) C.jlong {
	proxyHostText := C.bilolaGetStringUTF(env, proxyHostValue)
	serverNameText := C.bilolaGetStringUTF(env, serverNameValue)
	hostHeaderText := C.bilolaGetStringUTF(env, hostHeaderValue)
	if proxyHostText == nil || serverNameText == nil || hostHeaderText == nil {
		C.bilolaReleaseStringUTF(env, proxyHostValue, proxyHostText)
		C.bilolaReleaseStringUTF(env, serverNameValue, serverNameText)
		C.bilolaReleaseStringUTF(env, hostHeaderValue, hostHeaderText)
		return 0
	}
	proxyHost := C.GoString(proxyHostText)
	serverName := C.GoString(serverNameText)
	hostHeader := C.GoString(hostHeaderText)
	C.bilolaReleaseStringUTF(env, proxyHostValue, proxyHostText)
	C.bilolaReleaseStringUTF(env, serverNameValue, serverNameText)
	C.bilolaReleaseStringUTF(env, hostHeaderValue, hostHeaderText)
	engine, err := xhttpbridge.New(xhttpbridge.Config{
		Host:               proxyHost,
		Port:               int(proxyPort),
		ServerName:         serverName,
		HostHeader:         hostHeader,
		InsecureSkipVerify: insecure != 0,
		ConnectTimeout:     10 * time.Second,
		ProtectSocket:      buildProtect(env, clazz),
	})
	if err != nil {
		return 0
	}
	return storeEngine(engine)
}

//export Java_com_btun_client_GoBhttpBridge_nativeStart
func Java_com_btun_client_GoBhttpBridge_nativeStart(env *C.JNIEnv, clazz C.jclass, handle C.jlong) C.jint {
	engine := engineByID(int64(handle))
	if engine == nil {
		return -1
	}
	port, err := engine.Start()
	if err != nil {
		return -1
	}
	return C.jint(port)
}

//export Java_com_btun_client_GoBhttpBridge_nativeClose
func Java_com_btun_client_GoBhttpBridge_nativeClose(env *C.JNIEnv, clazz C.jclass, handle C.jlong) {
	id := int64(handle)
	enginesMu.Lock()
	engine := engines[id]
	delete(engines, id)
	enginesMu.Unlock()
	if engine != nil {
		engine.Close()
		engine.Wait()
	}
}

//export Java_com_btun_client_GoBhttpBridge_nativeUploadedBytes
func Java_com_btun_client_GoBhttpBridge_nativeUploadedBytes(env *C.JNIEnv, clazz C.jclass, handle C.jlong) C.jlong {
	if engine := engineByID(int64(handle)); engine != nil {
		return C.jlong(engine.UploadedBytes())
	}
	return 0
}

//export Java_com_btun_client_GoBhttpBridge_nativeDownloadedBytes
func Java_com_btun_client_GoBhttpBridge_nativeDownloadedBytes(env *C.JNIEnv, clazz C.jclass, handle C.jlong) C.jlong {
	if engine := engineByID(int64(handle)); engine != nil {
		return C.jlong(engine.DownloadedBytes())
	}
	return 0
}

//export Java_com_btun_client_GoBhttpBridge_nativeDrainLogs
func Java_com_btun_client_GoBhttpBridge_nativeDrainLogs(env *C.JNIEnv, clazz C.jclass, handle C.jlong) C.jstring {
	if engine := engineByID(int64(handle)); engine != nil {
		return javaString(env, engine.DrainLogs())
	}
	return javaString(env, "")
}

//export Java_com_btun_client_GoBhttpBridge_nativeLastError
func Java_com_btun_client_GoBhttpBridge_nativeLastError(env *C.JNIEnv, clazz C.jclass, handle C.jlong) C.jstring {
	if engine := engineByID(int64(handle)); engine != nil {
		return javaString(env, engine.LastError())
	}
	return javaString(env, "native bridge is closed")
}

// GoXhttpBridge deliberately shares the lifecycle implementation, while the
// exported JNI names remain tied to the Java class expected by Android.

//export Java_com_btun_client_GoXhttpBridge_nativeStart
func Java_com_btun_client_GoXhttpBridge_nativeStart(env *C.JNIEnv, clazz C.jclass, handle C.jlong) C.jint {
	return Java_com_btun_client_GoBhttpBridge_nativeStart(env, clazz, handle)
}

//export Java_com_btun_client_GoXhttpBridge_nativeClose
func Java_com_btun_client_GoXhttpBridge_nativeClose(env *C.JNIEnv, clazz C.jclass, handle C.jlong) {
	Java_com_btun_client_GoBhttpBridge_nativeClose(env, clazz, handle)
}

//export Java_com_btun_client_GoXhttpBridge_nativeUploadedBytes
func Java_com_btun_client_GoXhttpBridge_nativeUploadedBytes(env *C.JNIEnv, clazz C.jclass, handle C.jlong) C.jlong {
	return Java_com_btun_client_GoBhttpBridge_nativeUploadedBytes(env, clazz, handle)
}

//export Java_com_btun_client_GoXhttpBridge_nativeDownloadedBytes
func Java_com_btun_client_GoXhttpBridge_nativeDownloadedBytes(env *C.JNIEnv, clazz C.jclass, handle C.jlong) C.jlong {
	return Java_com_btun_client_GoBhttpBridge_nativeDownloadedBytes(env, clazz, handle)
}

//export Java_com_btun_client_GoXhttpBridge_nativeDrainLogs
func Java_com_btun_client_GoXhttpBridge_nativeDrainLogs(env *C.JNIEnv, clazz C.jclass, handle C.jlong) C.jstring {
	return Java_com_btun_client_GoBhttpBridge_nativeDrainLogs(env, clazz, handle)
}

//export Java_com_btun_client_GoXhttpBridge_nativeLastError
func Java_com_btun_client_GoXhttpBridge_nativeLastError(env *C.JNIEnv, clazz C.jclass, handle C.jlong) C.jstring {
	return Java_com_btun_client_GoBhttpBridge_nativeLastError(env, clazz, handle)
}

func main() {}
