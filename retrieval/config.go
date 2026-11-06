package retrieval

import "time"

// ConfigureFromEnv validates deployment limits without contacting any model service.
func (s *Service) ConfigureFromEnv() error {
	deadline, err := envRange("AUTOBID_INDEX_BUILD_DEADLINE_SECONDS", 7200, 1, 86400)
	if err != nil {
		return err
	}
	channel, err := envRange("AUTOBID_RETRIEVAL_CHANNEL_TIMEOUT_SECONDS", 18, 1, 19)
	if err != nil {
		return err
	}
	rerank, err := envRange("AUTOBID_RERANKER_MAX_CANDIDATES", 100, 1, 100)
	if err != nil {
		return err
	}
	s.IndexBuildDeadline, s.ChannelTimeout, s.MaxRerank = time.Duration(deadline)*time.Second, time.Duration(channel)*time.Second, rerank
	return nil
}
