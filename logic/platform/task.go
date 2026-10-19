package platform

import "context"

type Course struct {
	Name   string
	Detail string
}

type Account struct {
	Courses []Course
	Study   func(context.Context) error
	Close   func()
}
