package auth

// WithCatalogRegistration fences a registry publication against credential deletion,
// replacement and disablement. The callback must not call back into Manager.
func (m *Manager) WithCatalogRegistration(id string, epoch uint64, publish func(*Auth)) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a := m.auths[id]
	if a == nil || a.Disabled || a.RegistrationEpoch != epoch {
		return false
	}
	publish(a.Clone())
	return true
}
