# **Media Workers with Web Sockets**

## Using Web Sockets to transport realtime video

This is an internal Cloudflare Media Platform project.

A previous slide presentation (`~/src/let-it-slide/media-workers`) described building a system of modular components with Cloudflare Workers bindings to allow both the team and customers to build novel solutions programmatically.

Two particular example use cases were described:

* Take a livestream video input, use video-rate tracking to identify the tips of the weapons in a fencing duel, render the tracks of the weapon tips and overlay it on top of the video to provide a cool visualization. The composite video could be streamed to a Cloudflare Stream Live Input for broadcast and recording.  
* Play back a video from Cloudflare Stream, use AI to segment the video clip into shots or scenes and create a web page describing the video clip, with a still frame stored in Cloudflare Images and a text description for each shot/scene.

It’s time to start building some proofs of concept, even though not all the pieces are quite there yet, e.g. access to the required GPU resources. However, it seems pretty clear that many or all of the processing components that require hardware access will be implemented as a Cloudflare Container with a Web Socket interface to handle the video input/output. And the core binary used inside these containers is likely to be ffmpeg.

So let’s think about how these two use cases could be implemented, and how to build incremental proofs of concept without yet having the hardware acceleration.

# **Adding an overlay to a livestream**

We won’t even worry about the tracking part to begin with. Let’s just try to add an overlay rendered with something like the HTML5 Javascript Canvas API. And as a first step, let’s see if we can just pass video through from a web browser to a Stream Live output, using an application in a Cloudflare Container and Cloudflare Web Sockets for input/output.

## **Plan**

* The “livestream” part is a web browser sending the output of a webcam over a WebSocket connection that has been created as part of the Cloudflare application, one packet at a time.  
* The Cloudflare application in this case is a binary running in a Cloudflare Container, that opens one or more web sockets for input/output.  
* First implementation will be a “Pass Through” application, that uses ffmpeg to pass a video stream from input to output:  
  * The websocket interface accepts one packet of data at a time and sends it to ffmpeg input via a pipe.  
  * So this looks like a loop:  
    * Web Socket receives data packet.  
    * Data packet is written to the ffmpeg pipe.  
    * Repeat.  
  * The heart of the application is an ffmpeg process, reading from the input pipe and writing an RTMP or WebRTC stream that can be sent to a Cloudflare Stream Live Input.  
* Second implementation will be a “Compositor” application, that accepts two video streams via a websocket and creates a combined output stream.  
  * The websocket interface accepts one packet of data at a time and sends it to ffmpeg input via pipe(s).  
  * So this looks like a loop:  
    * Web Socket receives data packet.  
    * Data packet is written to the ffmpeg pipe.  
    * Repeat.  
  * There will be two inputs (differentiated by parameter in the input function?) to the web socket.  
  * The heart of the compositor is an ffmpeg process, reading from the two input pipes (is this possible?) and writing a combined output.  
  * The output should be an RTMP or WebRTC stream that can be sent to a Cloudflare Stream Live Input.  
  * TBD what the second video stream is. This might also be an incremental process:  
    * Start with a simple image with transparency/alpha? That allows us to get the Compositor up and running before we have to worry about the second video stream input.  
    * End up with a dynamically generated image with transparency rendered via something like the HTML5 Javascript Canvas API? Or the Cairo library? Is this even possible in a Cloudflare Worker? Is there any useful/interesting functionality in Cloudflare Images?

# **Generating a web page of scene stills and description of a video**

## **Plan**

* We’ll generate the video stream from a video-on-demand (VOD) stored in the Cloudflare Stream product, using a Cloudflare Worker.  
  * The Worker acts like a headless video player.  
  * It reads the m3u8 manifest from Cloudflare.  
  * It fetches video segments one by one, and sends them to our “Analyser” application via a web socket.  
* The Analyzer works similarly to the Pass Through and Compositor described above, at  high level:  
  * The websocket interface accepts one packet of data at a time and sends it to ffmpeg input via pipe(s).  
  * So this looks like a loop:  
    * Web Socket receives data packet.  
    * Data packet is written to the ffmpeg pipe.  
    * Repeat.  
  * The heart of the application is an ffmpeg process, reading from the input pipe.  
* Unlike the Pass Through/Compositor, the output of the Analyzer is a set of still images stored in Cloudflare Images, and a web page that displays those images along with some descriptive text.  
* As viewed externally, the Analyzer outputs JSON with the stored image IDs/URLs and description text.  
* A Cloudflare Worker reads the JSON and creates the final web page.  
* The Analyzer will be built in stages, starting simple and adding functionality incrementally:  
  * Version 1: Saves one image for every second of input. The “description” is just the ID/URL of each image.  
  * Version 2: Sends each image to an LLM in Cloudflare Workers to generate a better text description.

# **Questions**

* Is the proposed plan overall feasible?  
* Could you implement this, one stage at a time, given this description so far?  
* Is passing video stream data over web sockets going to be performant enough?  
* What programming language can we use for the logic to run ffmpeg and pipe data to it from the web socket in the container?  
* How do we handle the case of two data streams input to ffmpeg for the compositing case?  
* Any other concerns?