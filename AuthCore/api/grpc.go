package api

import (
	"AuthCore/proto"
	"context"
)

type GRPCServer struct {
	proto.UnimplementedAuthServiceServer
}

func (s *GRPCServer) CheckToken(c context.Context, t *proto.Token) (*proto.UserData, error) {
	resp, err := userInfo(c, t.Token)
	if err != nil {
		return nil, err
	}
	return &proto.UserData{
		UserId:      resp.UserID,
		Email:       resp.Email,
		PhoneNumber: resp.PhoneNumber,
		Gender:      resp.Gender == "M",
		FirstName:   resp.FirstName,
		LastName:    resp.LastName,
	}, nil
}
