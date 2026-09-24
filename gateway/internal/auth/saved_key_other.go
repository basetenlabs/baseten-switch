//go:build !darwin

package auth

func platformSavedAPIKeyStore() savedAPIKeyStore {
	return fileSavedAPIKeyStore{}
}
