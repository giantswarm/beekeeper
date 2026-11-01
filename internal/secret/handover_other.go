//go:build !linux

package secret

import "errors"

func (k *Keeper) PrepareHandover() error {
	if !k.State().Unlocked {
		return nil
	}
	return errors.New("vault handover requires systemd; restart refused")
}

func (k *Keeper) RestoreHandover() error { return nil }

func (k *Keeper) RevokeHandover() error { return nil }
