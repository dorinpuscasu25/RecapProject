package identity

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dorinpuscasu25/recapproject/internal/platform/apperr"
	"github.com/dorinpuscasu25/recapproject/internal/platform/httpx"
	"github.com/dorinpuscasu25/recapproject/internal/platform/principal"
)

// Handler exposes the identity HTTP API.
type Handler struct {
	Svc        *Service
	TrustProxy bool
}

func (h *Handler) client(r *http.Request) ClientInfo {
	ip := httpx.ClientIP(r, h.TrustProxy)
	return ClientInfo{IP: ip, IPPrefix: httpx.IPPrefix(ip), UserAgent: r.UserAgent()}
}

// PublicRoutes are unauthenticated identity endpoints.
func (h *Handler) PublicRoutes(r chi.Router) {
	r.Post("/auth/register", h.register)
	r.Post("/auth/verify-email", h.verifyEmail)
	r.Post("/auth/resend-verification", h.resend)
	r.Post("/auth/login", h.login)
	r.Post("/auth/refresh", h.refresh)
	r.Post("/auth/forgot-password", h.forgot)
	r.Post("/auth/reset-password", h.reset)
	r.Get("/auth/jwks", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, http.StatusOK, h.Svc.Tokens().PublicJWKS())
	})
}

// AuthedRoutes require an authenticated, active principal.
func (h *Handler) AuthedRoutes(r chi.Router) {
	r.Post("/auth/logout", h.logout)
	r.Post("/auth/logout-all", h.logoutAll)
	r.Post("/auth/change-password", h.changePassword)
	r.Get("/users/me", h.me)
	r.Patch("/users/me", h.updateMe)
	r.Get("/users/me/sessions", h.sessions)
	r.Delete("/users/me/sessions/{id}", h.revokeSession)
	r.Post("/users/me/email-change", h.emailChange)
	r.Post("/users/me/email-change/confirm", h.emailChangeConfirm)
	r.Post("/users/me/data-export", h.export)
	r.Get("/users/me/data-requests", h.dataRequests)
	r.Get("/users/me/data-requests/{id}/download", h.downloadExport)
	r.Post("/users/me/deletion", h.deletion)
}

// AdminRoutes require users:manage.
func (h *Handler) AdminRoutes(r chi.Router) {
	r.Get("/admin/users/{id}", h.adminGetUser)
	r.Put("/admin/users/{id}/roles", h.adminSetRoles)
	r.Put("/admin/users/{id}/status", h.adminSetStatus)
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var in RegisterInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	res, err := h.Svc.Register(r.Context(), in, h.client(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, res)
}

type authResponse struct {
	*TokenPair
	User *User `json:"user"`
}

func (h *Handler) verifyEmail(w http.ResponseWriter, r *http.Request) {
	var in VerifyInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	ci := h.client(r)
	ci.DeviceLabel = deviceLabel(in.DeviceLabel)
	pair, u, err := h.Svc.VerifyEmail(r.Context(), in, ci)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, authResponse{TokenPair: pair, User: u})
}

func (h *Handler) resend(w http.ResponseWriter, r *http.Request) {
	var in ResendInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := h.Svc.ResendVerification(r.Context(), in, h.client(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "accepted",
		"message": "If a pending account exists for this address, a new code has been sent."})
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var in LoginInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	pair, u, err := h.Svc.Login(r.Context(), in, h.client(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, authResponse{TokenPair: pair, User: u})
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	var in RefreshInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	pair, err := h.Svc.Refresh(r.Context(), in, h.client(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pair)
}

func (h *Handler) forgot(w http.ResponseWriter, r *http.Request) {
	var in ForgotInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := h.Svc.ForgotPassword(r.Context(), in, h.client(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "accepted",
		"message": "If an account exists for this address, a reset code has been sent."})
}

func (h *Handler) reset(w http.ResponseWriter, r *http.Request) {
	var in ResetInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := h.Svc.ResetPassword(r.Context(), in, h.client(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if err := h.Svc.Logout(r.Context(), principal.From(r.Context()), h.client(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) logoutAll(w http.ResponseWriter, r *http.Request) {
	if err := h.Svc.LogoutAll(r.Context(), principal.From(r.Context()), h.client(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	var in ChangePasswordInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := h.Svc.ChangePassword(r.Context(), principal.From(r.Context()), in, h.client(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	u, err := h.Svc.GetUser(r.Context(), principal.From(r.Context()).ID)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}

func (h *Handler) updateMe(w http.ResponseWriter, r *http.Request) {
	var in UpdateMeInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	u, err := h.Svc.UpdateMe(r.Context(), principal.From(r.Context()), in)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}

func (h *Handler) sessions(w http.ResponseWriter, r *http.Request) {
	list, err := h.Svc.ListSessions(r.Context(), principal.From(r.Context()))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (h *Handler) revokeSession(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, apperr.NotFound("session_not_found", "Session not found."))
		return
	}
	if err := h.Svc.RevokeSession(r.Context(), principal.From(r.Context()), id, h.client(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.NoContent(w)
}

func (h *Handler) emailChange(w http.ResponseWriter, r *http.Request) {
	var in EmailChangeInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := h.Svc.InitiateEmailChange(r.Context(), principal.From(r.Context()), in, h.client(r)); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "verification_required",
		"message": "If the new address can be used, a confirmation code has been sent to it. Your current email stays active until confirmed."})
}

func (h *Handler) emailChangeConfirm(w http.ResponseWriter, r *http.Request) {
	var in EmailChangeConfirmInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	u, err := h.Svc.ConfirmEmailChange(r.Context(), principal.From(r.Context()), in, h.client(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}

func (h *Handler) export(w http.ResponseWriter, r *http.Request) {
	req, err := h.Svc.RequestExport(r.Context(), principal.From(r.Context()))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, req)
}

func (h *Handler) dataRequests(w http.ResponseWriter, r *http.Request) {
	list, err := h.Svc.ListDataRequests(r.Context(), principal.From(r.Context()))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": list})
}

func (h *Handler) downloadExport(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, apperr.NotFound("data_request_not_found", "Data request not found."))
		return
	}
	rc, n, err := h.Svc.OpenExport(r.Context(), principal.From(r.Context()), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="recap-export-`+id.String()+`.json"`)
	w.Header().Set("Cache-Control", "private, no-store")
	if n > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
	}
	_, _ = io.Copy(w, rc)
}

func (h *Handler) deletion(w http.ResponseWriter, r *http.Request) {
	var in DeletionInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	req, err := h.Svc.RequestDeletion(r.Context(), principal.From(r.Context()), in, h.client(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, req)
}

func (h *Handler) adminGetUser(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, apperr.NotFound("user_not_found", "User not found."))
		return
	}
	u, err := h.Svc.GetUser(r.Context(), id)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}

func (h *Handler) adminSetRoles(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, apperr.NotFound("user_not_found", "User not found."))
		return
	}
	var in struct {
		Roles []string `json:"roles"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	u, err := h.Svc.SetRoles(r.Context(), principal.From(r.Context()), id, in.Roles)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}

func (h *Handler) adminSetStatus(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, apperr.NotFound("user_not_found", "User not found."))
		return
	}
	var in struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if err := h.Svc.SetStatus(r.Context(), principal.From(r.Context()), id, in.Status, in.Reason); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.NoContent(w)
}

// ---------- Middleware ----------

// Authenticate requires a valid bearer token (or service API key) and installs the principal.
func (h *Handler) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var (
			p   *principal.Principal
			err error
		)
		if key := r.Header.Get("X-API-Key"); key != "" {
			p, err = h.Svc.AuthenticateServiceKey(r.Context(), key)
		} else {
			authz := r.Header.Get("Authorization")
			scheme, tok, ok := strings.Cut(authz, " ")
			if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
				httpx.WriteError(w, r, apperr.Unauthenticated("auth.required", "Authentication is required."))
				return
			}
			p, err = h.Svc.Authenticate(r.Context(), strings.TrimSpace(tok))
		}
		if err != nil {
			httpx.WriteError(w, r, err)
			return
		}
		ip := httpx.ClientIP(r, h.TrustProxy)
		p.IP, p.IPPrefix = ip, httpx.IPPrefix(ip)
		next.ServeHTTP(w, r.WithContext(principal.With(r.Context(), p)))
	})
}

// RequireUser rejects service principals on consumer/self-service routes.
func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := principal.From(r.Context()); p == nil || p.Kind != principal.KindUser {
			httpx.WriteError(w, r, apperr.Forbidden("auth.user_required", "This endpoint requires a user account."))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Require enforces that the principal holds the permission.
func Require(perm string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !principal.From(r.Context()).Can(perm) {
				httpx.WriteError(w, r, apperr.Forbidden("auth.forbidden", "You do not have permission to perform this action.").
					WithDetail("required_permission", perm))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
