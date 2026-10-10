package convert

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif" // registers the GIF decoder
	_ "image/jpeg"
	"image/png"
	"slices"

	"github.com/urmzd/saige/agent/types"
)

// transcodable are the image formats the standard library decodes.
var transcodable = []types.MediaType{types.MediaPNG, types.MediaJPEG, types.MediaGIF}

// transcode re-encodes an image the target does not take as PNG.
type transcode struct{}

// Transcode returns a convert converter that re-encodes a JPEG, PNG or GIF
// image as PNG for a target that takes PNG but not the image's own format,
// such as a GIF for a model that reads only JPEG and PNG. It keeps the
// first frame of an animated GIF.
func Transcode() types.Converter { return transcode{} }

func (transcode) Name() string                         { return "transcode" }
func (transcode) Version() string                      { return "1.png" }
func (transcode) Action() types.ModalityAction         { return types.ActConvert }
func (transcode) Produces(types.Part) []types.Modality { return []types.Modality{types.ModalityImage} }

func (transcode) Accepts(p types.Part) bool {
	src, ok := types.SourceOf(p)
	if !ok || len(src.Inline) == 0 {
		return false
	}
	_, img := p.(types.ImagePart)
	return img && slices.Contains(transcodable, baseType(src.MediaType))
}

// Fits implements types.TargetedConverter: the target must take PNG.
func (transcode) Fits(_ types.Part, target types.Offering) bool {
	_, ok := target.Modalities.Accepts(types.MediaPNG)
	return ok
}

func (transcode) Estimate(types.Part, types.Offering) (types.ConversionEstimate, error) {
	return types.ConversionEstimate{}, nil
}

func (transcode) Convert(_ context.Context, p types.Part, _ types.ConvertEnv) ([]types.Part, types.ConversionUsage, error) {
	img := p.(types.ImagePart)
	decoded, _, err := image.Decode(bytes.NewReader(img.Source.Inline))
	if err != nil {
		return nil, types.ConversionUsage{}, fmt.Errorf("decode %s: %w", img.Source.MediaType, err)
	}
	var b bytes.Buffer
	if err := png.Encode(&b, decoded); err != nil {
		return nil, types.ConversionUsage{}, fmt.Errorf("encode png: %w", err)
	}
	src := types.Bytes(types.MediaPNG, b.Bytes())
	src.Filename = img.Source.Filename
	out := types.ImagePart{Source: src, ImageMeta: img.ImageMeta}
	bounds := decoded.Bounds()
	out.Width, out.Height = bounds.Dx(), bounds.Dy()
	return []types.Part{out}, types.ConversionUsage{}, nil
}
