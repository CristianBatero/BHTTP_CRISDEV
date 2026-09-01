//go:build linux && cgo

package btun

/*
#cgo LDFLAGS: -lpam
#include <security/pam_appl.h>
#include <stdlib.h>
#include <string.h>

struct btun_credentials {
	const char *username;
	const char *password;
};

static int btun_conversation(int count, const struct pam_message **messages,
		struct pam_response **responses, void *opaque) {
	if (count <= 0 || messages == NULL || responses == NULL || opaque == NULL) {
		return PAM_CONV_ERR;
	}
	struct btun_credentials *credentials = (struct btun_credentials *)opaque;
	struct pam_response *result = calloc((size_t)count, sizeof(struct pam_response));
	if (result == NULL) {
		return PAM_BUF_ERR;
	}
	for (int index = 0; index < count; index++) {
		const char *answer = NULL;
		switch (messages[index]->msg_style) {
		case PAM_PROMPT_ECHO_ON:
			answer = credentials->username;
			break;
		case PAM_PROMPT_ECHO_OFF:
			answer = credentials->password;
			break;
		case PAM_ERROR_MSG:
		case PAM_TEXT_INFO:
			break;
		default:
			for (int free_index = 0; free_index < index; free_index++) {
				free(result[free_index].resp);
			}
			free(result);
			return PAM_CONV_ERR;
		}
		if (answer != NULL) {
			result[index].resp = strdup(answer);
			if (result[index].resp == NULL) {
				for (int free_index = 0; free_index <= index; free_index++) {
					free(result[free_index].resp);
				}
				free(result);
				return PAM_BUF_ERR;
			}
		}
	}
	*responses = result;
	return PAM_SUCCESS;
}

static int btun_pam_authenticate(const char *service, const char *username,
		const char *password) {
	struct btun_credentials credentials = {username, password};
	struct pam_conv conversation = {btun_conversation, &credentials};
	pam_handle_t *handle = NULL;
	int status = pam_start(service, username, &conversation, &handle);
	if (status == PAM_SUCCESS) {
		status = pam_authenticate(handle, PAM_SILENT);
	}
	if (status == PAM_SUCCESS) {
		status = pam_acct_mgmt(handle, PAM_SILENT);
	}
	if (handle != NULL) {
		pam_end(handle, status);
	}
	return status;
}
*/
import "C"

import (
	"errors"
	"strings"
	"unsafe"
)

type PAMAuthenticator struct {
	Service string
}

func (auth PAMAuthenticator) Name() string { return "pam" }

func (auth PAMAuthenticator) Authenticate(username, password string) error {
	if username == "" || strings.IndexByte(username, 0) >= 0 || strings.IndexByte(password, 0) >= 0 {
		return errors.New("invalid username or password")
	}
	service := strings.TrimSpace(auth.Service)
	if service == "" {
		service = "login"
	}
	cService := C.CString(service)
	cUsername := C.CString(username)
	cPassword := C.CString(password)
	defer C.free(unsafe.Pointer(cService))
	defer C.free(unsafe.Pointer(cUsername))
	defer C.free(unsafe.Pointer(cPassword))
	if C.btun_pam_authenticate(cService, cUsername, cPassword) != C.PAM_SUCCESS {
		return errors.New("invalid username or password")
	}
	return nil
}
