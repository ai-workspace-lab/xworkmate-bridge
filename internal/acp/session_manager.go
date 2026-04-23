package acp

import (
	"context"

	"xworkmate-bridge/internal/shared"
)

func (s *Server) enqueue(threadID string, task task) (map[string]any, *shared.RPCError) {
	queue := s.ensureQueue(threadID)
	queue <- task
	result := <-task.done
	return result.response, result.err
}

func (s *Server) ensureQueue(threadID string) chan task {
	s.mu.Lock()
	defer s.mu.Unlock()
	queue, ok := s.queues[threadID]
	if ok {
		return queue
	}
	queue = make(chan task, 32)
	s.queues[threadID] = queue
	go s.runQueue(queue)
	return queue
}

func (s *Server) runQueue(queue chan task) {
	for task := range queue {
		response, err := s.executeSessionTask(task)
		task.done <- taskResult{response: response, err: err}
	}
}

func (s *Server) emitSessionUpdate(
	session *session,
	notify func(map[string]any),
	turnID string,
	payload map[string]any,
) {
	if notify == nil {
		return
	}
	s.mu.Lock()
	session.seq++
	seq := session.seq
	s.mu.Unlock()
	params := map[string]any{
		"sessionId": session.sessionID,
		"threadId":  session.threadID,
		"turnId":    turnID,
		"seq":       seq,
	}
	for key, value := range payload {
		params[key] = value
	}
	notify(shared.NotificationEnvelope("session.update", params))
}

func (s *Server) getOrCreateSession(sessionID, threadID string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, ok := s.sessions[sessionID]; ok {
		if threadID != "" {
			session.threadID = threadID
		}
		session.closed = false
		return session
	}
	session := &session{sessionID: sessionID, threadID: threadID}
	s.sessions[sessionID] = session
	return session
}

func (s *Server) resetSession(sessionID, threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sessionID] = &session{
		sessionID: sessionID,
		threadID:  threadID,
		history:   []string{},
	}
}

func (s *Server) setSessionCancel(sessionID string, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, ok := s.sessions[sessionID]; ok {
		session.cancel = cancel
	}
}

func (s *Server) clearSessionCancel(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session, ok := s.sessions[sessionID]; ok {
		session.cancel = nil
	}
}

func (s *Server) cancelSession(sessionID string) bool {
	s.mu.Lock()
	session, ok := s.sessions[sessionID]
	if !ok {
		s.mu.Unlock()
		return false
	}
	cancel := session.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		return true
	}
	return false
}

func (s *Server) closeSession(sessionID string) bool {
	s.mu.Lock()
	session, ok := s.sessions[sessionID]
	if !ok {
		s.mu.Unlock()
		return false
	}
	cancel := session.cancel
	session.closed = true
	delete(s.sessions, sessionID)
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return true
}
