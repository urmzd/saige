package tree

import (
	"context"
	"fmt"
	"testing"

	"github.com/urmzd/saige/agent/types"
)

func BenchmarkAddChild(b *testing.B) {
	tr, _ := New(types.NewSystemMessage("system"))
	root := tr.Root()

	b.ResetTimer()
	parent := root
	for i := 0; i < b.N; i++ {
		child, _ := tr.AddChild(context.Background(), parent.ID, types.NewUserMessage(fmt.Sprintf("msg-%d", i)))
		parent = child
	}
}

func BenchmarkFlattenBranch(b *testing.B) {
	for _, depth := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			tr, _ := New(types.NewSystemMessage("system"))
			parent := tr.Root()
			for i := 0; i < depth; i++ {
				child, _ := tr.AddChild(context.Background(), parent.ID, types.NewUserMessage(fmt.Sprintf("msg-%d", i)))
				parent = child
			}

			branch := tr.Active()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tr.FlattenBranch(branch)
			}
		})
	}
}

func BenchmarkBranch(b *testing.B) {
	tr, _ := New(types.NewSystemMessage("system"))
	root := tr.Root()
	user, _ := tr.AddChild(context.Background(), root.ID, types.NewUserMessage("hello"))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr.Branch(context.Background(), user.ID, fmt.Sprintf("branch-%d", i), types.NewUserMessage("branched"))
	}
}
