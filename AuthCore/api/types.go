package api

import "time"

type SignUpRequest struct {
	Email       string `json:"email"`
	PhoneNumber string `json:"phone_number"`
	Gender      string `json:"gender"`
	FirstName   string `json:"first_name"`
	LastName    string `json:"last_name"`
	Password    string `json:"password"`
}

type SignInRequest struct {
	Email       string `json:"email"`
	PhoneNumber string `json:"phone_number"`
	Password    string `json:"password"`
}

type SignInResponse struct {
	RefreshToken string    `json:"refresh_token"`
	AccessToken  string    `json:"access_token"`
	Expiration   time.Time `json:"expiration"`
}

type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type RefreshResponse struct {
	AccessToken string    `json:"access_token"`
	Expiration  time.Time `json:"expiration"`
}

type UserInfoResponse struct {
	UserID      int64  `json:"user_id"`
	Email       string `json:"email"`
	PhoneNumber string `json:"phone_number"`
	Gender      string `json:"gender"`
	FirstName   string `json:"first_name"`
	LastName    string `json:"last_name"`
}

type UserClaim struct {
	IsRefresh   bool   `json:"is_refresh"`
	UserID      int64  `json:"user_id"`
	Email       string `json:"email"`
	PhoneNumber string `json:"phone_number"`
}
