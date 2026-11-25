# Bàn giao tính năng secure chat cho croc

Tài liệu này mô tả phần đã triển khai trong working tree ngày 25/11/2026 để người tiếp theo có thể đọc mã, chạy thử và hoàn tất nghiệm thu. Đây là bản phát triển, chưa có commit, pull request hoặc bản phát hành. Cả hai người dùng phải chạy bản croc có lệnh `chat`; bản croc phát hành trước đó không hiểu giao thức này.

## 1. Mục tiêu và phạm vi đã làm

Đã thêm trò chuyện mã hóa đầu cuối giữa hai terminal bằng lệnh `croc chat`. Người tạo phòng nhận mã mời sáu từ; người còn lại chạy `croc chat --join` và nhập mã tại lời nhắc ẩn. Hai máy có thể dùng relay công khai hiện có hoặc cùng chỉ định relay riêng. Relay chỉ chuyển tiếp dữ liệu, không cần sửa mã relay để hiểu chat.

Phiên bản này chỉ hỗ trợ một cuộc trò chuyện 1:1, văn bản UTF-8, không lưu lịch sử vào ứng dụng. Không có nhóm, gửi tệp trong chat, tự kết nối lại hay chuyển tiếp tin khi mất mạng. Mỗi tin tối đa 16 KiB; phiên tối đa hai giờ, relay có thể ngắt sớm hơn. Mã mời là bí mật: người biết mã có thể thử tham gia phòng.

## 2. Những thay đổi so với croc ban đầu

| Thành phần | Đã bổ sung hoặc sửa |
| --- | --- |
| CLI | Lệnh `chat`, tùy chọn `--join`, hỗ trợ `--relay`/`--relay6`, mật khẩu relay và SOCKS5/HTTP CONNECT; kiểm tra terminal tương tác và từ chối cờ không áp dụng. |
| Mã mời | Sinh và phân tích mã sáu từ. Hai từ đầu xác định phòng relay trong namespace riêng của chat; bốn từ sau là mật khẩu PAKE. Mã cũng chọn relay công khai nhất quán cho hai phía. |
| Xác thực | Tái sử dụng PAKE của croc, thêm mục đích `peer-chat-v1` và ràng buộc phiên với tên phòng, vai trò, tham số và transcript. Hai phía xác nhận cùng khóa trước khi trao đổi tin. |
| Khóa tin nhắn | Từ khóa phiên PAKE và salt, HKDF-SHA256 dẫn xuất hai khóa AES-256-GCM riêng: host → joiner và joiner → host. |
| Giao thức tin | Bản ghi có số thứ tự tăng liên tục, nonce ngẫu nhiên và tag xác thực. Có `READY`, `TEXT`, `ACK`, `PING`, `PONG`, `CLOSE`; giới hạn kích thước và từ chối replay, bản ghi sai thứ tự hoặc sai xác thực. |
| Phiên chat | Gửi/nhận đồng thời, queue hữu hạn, timeout cho handshake, ghi, ACK, heartbeat và đóng kết nối; hủy phiên, CLOSE và trạng thái giao tin chưa chắc chắn. |
| Terminal | Nhập mã mời ẩn, soạn và gửi tin, hiển thị tin đến khi đang gõ, lệnh `/help`, `/clear`, `/quit`, Ctrl+C/Ctrl+U. Lọc ký tự điều khiển và dấu điều hướng văn bản trước khi hiển thị. |
| Kết nối dùng chung | Sửa `src/comm/comm.go` để thao tác đọc không xóa deadline ghi của luồng ghi đồng thời. |
| Tài liệu/CI | README, man page, đặc tả, hướng dẫn kiểm thử và các job kiểm thử chat trên Linux, Windows, macOS. |

PAKE và giao thức truyền tệp/SSH cũ không bị thay thuật toán. `peer-chat-v1` là nhãn mục đích mới ở phía hai client, không phải nhãn relay cần biết. ACK chỉ chứng minh client bên kia đã nhận và chấp nhận bản ghi; không chứng minh người dùng đã đọc. Nếu thiếu ACK, giao diện báo trạng thái chưa chắc chắn và không tự gửi lại.

## 3. Luồng hoạt động ngắn gọn

1. Host sinh mã mời sáu từ, vào phòng relay. Joiner dùng cùng mã để tìm phòng và vào relay; người thứ ba bị relay từ chối khi phòng đã có hai client.
2. Joiner gửi PAKE A; host trả PAKE B cùng salt. Hai phía dẫn xuất khóa gốc và trao đổi hai thông điệp xác nhận khóa. Sai mã mời hoặc sai tham số khiến handshake thất bại.
3. Hai phía dẫn xuất khóa riêng cho từng chiều rồi trao đổi bản ghi `READY` đã mã hóa. Chỉ sau bước này UI mới cho gửi `TEXT`.
4. Mỗi `TEXT` được mã hóa và xác thực bằng AES-GCM. Bên nhận gửi `ACK` tham chiếu số thứ tự của `TEXT`. Hai phía trao đổi `PING`/`PONG` để phát hiện kết nối mất phản hồi và dùng `CLOSE` khi kết thúc bình thường.

Định dạng từng byte, nhãn HKDF, giới hạn và sơ đồ trạng thái nằm trong [SECURE_CHAT_SPEC.md](SECURE_CHAT_SPEC.md). Code chính của giao thức ở `src/chat/`.

## 4. Bản đồ mã nguồn

| Đường dẫn | Trách nhiệm |
| --- | --- |
| `src/chat/handshake.go`, `keys.go` | PAKE, xác nhận khóa, dẫn xuất khóa cho từng chiều. |
| `src/chat/protocol.go`, `record.go` | Loại bản ghi, mã hóa/giải mã, kiểm tra phiên bản, thứ tự và giới hạn. |
| `src/chat/api.go`, `session.go` | API `StartHost`, `Join`, `SendText`, `Events`, `Close`, `Wait`; vòng đời phiên và timeout. |
| `src/codephrase/codephrase.go`, `src/pakekey/pakekey.go` | Mã mời chat và nhãn mục đích PAKE. |
| `src/cli/chat.go`, `chat_ui.go`, `chat_input_*.go`, `chat_console_*.go` | Lệnh, giao diện terminal, đọc phím POSIX/Windows. |
| `src/comm/comm.go` | Deadline đọc/ghi độc lập. |
| `scripts/test-chat-pty.py` | Kiểm thử hai tiến trình CLI thật qua relay nội bộ trên POSIX. |
| `.github/workflows/ci.yml` | Job kiểm thử chat đã được thêm nhưng chưa chạy trên GitHub Actions. |

## 5. Chạy thử và kiểm thử

Từ thư mục gốc repo, build bản hiện tại:

```bash
go build -o /tmp/croc-chat .
```

Terminal 1 chạy `/tmp/croc-chat chat`. Terminal 2 chạy `/tmp/croc-chat chat --join`, dán mã sáu từ terminal 1 vào lời nhắc ẩn rồi Enter. Đợi cả hai hiện `Secure chat connected`, gửi thử tin hai chiều và dùng `/quit` để thoát. Hai lệnh này dùng relay công khai; cần mạng tới relay. Để thử hoàn toàn cục bộ, xem [TESTING.md](TESTING.md#secure-chat) để chạy relay nội bộ bằng terminal thứ ba.

Các lệnh kiểm thử chính:

```bash
go test ./...
go test -race ./src/chat ./src/comm
go test -race ./src/cli -run Chat
go test -tags croc_no_tailcat ./src/chat ./src/cli
go vet ./src/chat ./src/cli ./src/comm
python3 scripts/test-chat-pty.py --binary /tmp/croc-chat
```

Trên Linux amd64 với Go 1.27.3, các kiểm thử trên đã đạt. Đã kiểm tra thêm fuzz decoder, vector HKDF/AES-GCM, handshake lỗi, replay, proxy SOCKS5/HTTP CONNECT, người thứ ba bị từ chối, relay công khai với hai tiến trình cùng máy, và cross-build Windows/macOS. Chi tiết bằng chứng và giới hạn kiểm thử ở [SECURE_CHAT_VALIDATION.md](SECURE_CHAT_VALIDATION.md).

## 6. Việc cần người tiếp theo hoàn tất

1. **Chạy thử native trên Windows.** Cross-build đã qua nhưng chưa kiểm tra console thật. Dùng hai hoặc ba terminal theo [TESTING.md](TESTING.md#secure-chat): nhập mã ẩn, tiếng Việt/emoji, phím Backspace/Ctrl+U/Ctrl+C, tin đến khi đang soạn, thoát và xác nhận chế độ echo được khôi phục.
2. **Chạy thử native trên macOS.** Chạy script PTY và kiểm tra terminal thủ công trên máy macOS. Cross-build chưa chứng minh hành vi terminal lúc chạy.
3. **Thử hai máy ở hai mạng khác nhau qua relay công khai.** Test hiện tại qua relay công khai dùng hai tiến trình trên cùng một máy Linux. Ghi lại hệ điều hành, revision và kết quả, không ghi mã mời hay nội dung chat.
4. **Đưa mã lên nhánh để CI chạy trên GitHub Actions, rồi xem kết quả từng job.** File workflow đã sửa tại local nhưng chưa được kiểm chứng trên runner. Trước khi bàn giao qua Git, nhớ thêm cả các tệp `src/chat/`, CLI và tài liệu đang ở trạng thái untracked; chỉ `git diff` sẽ không hiển thị nội dung của chúng. Nếu được yêu cầu push, tuân theo `AGENTS.md`: dùng `~/dotfiles/git-pr '<conventional commit message>'`, không chạy `git push` trực tiếp.
5. **Xử lý lỗi phát hiện trong các bước trên, cập nhật bằng chứng nghiệm thu.** Chưa có đánh giá bảo mật độc lập. Chỉ cân nhắc công bố tính năng sau khi kiểm tra vận hành và review giao thức/terminal xong.

Không có commit, PR, tag hay release nào được tạo cho phần triển khai này. Bản build Windows/macOS phục vụ nghiệm thu thủ công, nếu còn trên máy phát triển, nằm ở `/tmp/croc-chat-native-validation/`; đây là artifact tạm, không thuộc repo và không thay thế bản build từ revision đã chia sẻ.
