package infra

func UpstreamHeadersByServicePort(services []ServiceEndpoint, headers map[string]string) map[uint32]map[string]string {
	if len(headers) == 0 {
		return nil
	}
	result := make(map[uint32]map[string]string, len(services))
	for _, service := range services {
		if service.Port == 0 {
			continue
		}
		result[service.Port] = cloneStringMap(headers)
	}
	return result
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
