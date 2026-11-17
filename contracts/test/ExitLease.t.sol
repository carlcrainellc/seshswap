// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;
import {Test} from "forge-std/Test.sol";
import {XmrSwapV3} from "../src/XmrSwapV3.sol";
import {SeshRegistry} from "../src/registry/SeshRegistry.sol";
import {ExitLease} from "../src/exits/ExitLease.sol";
import {MockUSDC} from "../src/MockUSDC.sol";
import {MockAggregatorV3} from "../src/MockAggregatorV3.sol";

contract RejectEthPayout {
    receive() external payable {
        revert();
    }
}

contract ReenterPayout {
    ExitLease public el;
    uint256 public id;
    bool public tried;
    bool public reverted;

    function arm(ExitLease e, uint256 i) external {
        el = e;
        id = i;
    }

    receive() external payable {
        if (tried) return;
        tried = true;
        try el.buy{value: msg.value}(id, address(0), 1, msg.value, ExitLeaseTest_SESH.B) {}
        catch {
            reverted = true;
        }
    }
}

library ExitLeaseTest_SESH {
    string constant B = "bog6mtasrhyriq5b35qidik4e3earg9om8d989hhza3jaeb5oqwo.sesh";
}

/// ExitLease: offers bound to SeshRegistry "exit" listings, exact-price ETH and
/// USDC purchases paid straight to the seller, stacking hours, and no refunds.
contract ExitLeaseTest is Test {
    SeshRegistry reg;
    ExitLease el;
    MockUSDC usdc;

    address seller = address(0x5E11E2);
    address payout = address(0x9A70);
    address buyer = address(0xB0B);
    address other = address(0x07E2);

    string constant EXIT_SESH = "5tg4wjp6qg6pyw9kstkkcwihpqhjh1ydzixjo78pjdditdcpyxfo.sesh";
    string constant BSESH = "bog6mtasrhyriq5b35qidik4e3earg9om8d989hhza3jaeb5oqwo.sesh";
    uint128 constant WEI_H = 0.00001 ether;
    uint128 constant USDC_H = 100_000; // 0.10 USDC
    uint256 id;

    function setUp() public {
        vm.warp(1_000_000);
        usdc = new MockUSDC();
        MockAggregatorV3 feed = new MockAggregatorV3("ETH / USD", 3000e8);
        reg = new SeshRegistry(
            SeshRegistry.Params({
                swap: address(new XmrSwapV3()),
                siteMinBond: 0.0002 ether,
                makerMinBond: 0.002 ether,
                capFactor: 5,
                penaltyBps: 500,
                minPenalty: 0.00005 ether,
                challengeBond: 0.0001 ether,
                exitDelay: 3 days,
                challengeWindow: 2 days,
                minAttest: 10 minutes,
                maxAttest: 30 minutes,
                usdc: address(usdc),
                usdcDecimals: 6,
                ethUsdFeed: address(feed),
                maxFeedAge: 2 days
            })
        );
        el = new ExitLease(reg, address(usdc));
        vm.deal(seller, 1 ether);
        vm.deal(buyer, 1 ether);
        vm.deal(other, 1 ether);
        usdc.mint(buyer, 100e6);
        vm.prank(seller);
        id = reg.register{value: 0.0002 ether}(EXIT_SESH, "exit", "exit price=0.10usdc/h chain=arb-sepolia");
        vm.prank(seller);
        el.setOffer(id, payout, WEI_H, USDC_H, 24);
    }

    function test_buyEth_paysSellerAndRecords() public {
        uint256 amt = el.quote(id, address(0), 3);
        assertEq(amt, 3 * WEI_H);
        vm.expectEmit(true, true, true, true);
        emit ExitLease.Leased(0, id, buyer, payout, address(0), amt, 3, 1_000_000, 1_000_000 + 3 hours, BSESH);
        vm.prank(buyer);
        uint256 lid = el.buy{value: amt}(id, address(0), 3, amt, BSESH);
        assertEq(lid, 0);
        assertEq(payout.balance, amt);
        assertEq(address(el).balance, 0);
        ExitLease.Lease memory l = el.lease(0);
        assertEq(l.buyer, buyer);
        assertEq(l.payout, payout);
        assertEq(l.amount, amt);
        assertEq(l.end, 1_000_000 + 3 hours);
        assertEq(l.buyerSesh, BSESH);
        assertEq(el.activeUntil(id, BSESH), 1_000_000 + 3 hours);
        assertEq(el.leaseCount(), 1);
    }

    function test_buyUsdc_paysSellerDirect() public {
        vm.startPrank(buyer);
        usdc.approve(address(el), 2 * USDC_H);
        el.buy(id, address(usdc), 2, 2 * USDC_H, BSESH);
        vm.stopPrank();
        assertEq(usdc.balanceOf(payout), 2 * USDC_H);
        assertEq(usdc.balanceOf(address(el)), 0);
        assertEq(el.lease(0).asset, address(usdc));
    }

    function test_usdcWithEthAttached_reverts() public {
        vm.startPrank(buyer);
        usdc.approve(address(el), USDC_H);
        vm.expectRevert(ExitLease.WrongAmount.selector);
        el.buy{value: 1}(id, address(usdc), 1, USDC_H, BSESH);
        vm.stopPrank();
    }

    function test_usdcWithoutApproval_reverts() public {
        vm.prank(buyer);
        vm.expectRevert();
        el.buy(id, address(usdc), 1, USDC_H, BSESH);
    }

    function test_stacksHoursWhileActive() public {
        vm.startPrank(buyer);
        el.buy{value: WEI_H}(id, address(0), 1, WEI_H, BSESH);
        vm.warp(1_000_000 + 30 minutes);
        el.buy{value: 2 * WEI_H}(id, address(0), 2, 2 * WEI_H, BSESH);
        vm.stopPrank();
        ExitLease.Lease memory l = el.lease(1);
        assertEq(l.start, 1_000_000 + 1 hours);
        assertEq(l.end, 1_000_000 + 3 hours);
    }

    function test_freshStartAfterExpiry() public {
        vm.prank(buyer);
        el.buy{value: WEI_H}(id, address(0), 1, WEI_H, BSESH);
        vm.warp(1_000_000 + 5 hours);
        vm.prank(buyer);
        el.buy{value: WEI_H}(id, address(0), 1, WEI_H, BSESH);
        assertEq(el.lease(1).start, 1_000_000 + 5 hours);
        assertEq(el.lease(1).end, 1_000_000 + 6 hours);
    }

    function test_wrongAmount_reverts() public {
        vm.startPrank(buyer);
        vm.expectRevert(ExitLease.WrongAmount.selector);
        el.buy{value: WEI_H - 1}(id, address(0), 1, WEI_H, BSESH);
        vm.expectRevert(ExitLease.WrongAmount.selector);
        el.buy{value: WEI_H + 1}(id, address(0), 1, WEI_H + 1, BSESH);
        vm.stopPrank();
    }

    function test_priceRaiseBetweenQuoteAndBuy_reverts() public {
        uint256 q = el.quote(id, address(0), 1);
        vm.prank(seller);
        el.setOffer(id, payout, WEI_H * 2, USDC_H, 24);
        vm.prank(buyer);
        vm.expectRevert(ExitLease.WrongAmount.selector);
        el.buy{value: q}(id, address(0), 1, q, BSESH);
    }

    function test_tooManyHours_reverts() public {
        vm.prank(buyer);
        vm.expectRevert(ExitLease.TooManyHours.selector);
        el.buy{value: 25 * WEI_H}(id, address(0), 25, 25 * WEI_H, BSESH);
    }

    function test_zeroHours_reverts() public {
        vm.prank(buyer);
        vm.expectRevert(ExitLease.BadParams.selector);
        el.buy(id, address(0), 0, 0, BSESH);
    }

    function test_assetNotAccepted() public {
        vm.prank(seller);
        el.setOffer(id, payout, WEI_H, 0, 24);
        vm.expectRevert(ExitLease.AssetNotAccepted.selector);
        el.quote(id, address(usdc), 1);
        vm.expectRevert(ExitLease.AssetNotAccepted.selector);
        el.quote(id, address(0xdead), 1);
    }

    function test_badSesh_reverts() public {
        string[5] memory bad = [
            "short.sesh",
            "Bog6mtasrhyriq5b35qidik4e3earg9om8d989hhza3jaeb5oqwo.sesh", // uppercase
            "bog6mtasrhyriq5b35qidik4e3earg9om8d989hhza3jaeb5oqw0.sesh", // 0 not z-base32
            "bog6mtasrhyriq5b35qidik4e3earg9om8d989hhza3jaeb5oqwo.sess",
            "bog6mtasrhyriq5b35qidik4e3earg9om8d989hhza3jaeb5oqwl.sesh" // l not z-base32
        ];
        for (uint256 i = 0; i < bad.length; i++) {
            vm.prank(buyer);
            vm.expectRevert(ExitLease.BadSesh.selector);
            el.buy{value: WEI_H}(id, address(0), 1, WEI_H, bad[i]);
        }
    }

    function test_onlyOwnerSetsOffer() public {
        vm.prank(other);
        vm.expectRevert(ExitLease.NotOwner.selector);
        el.setOffer(id, other, 1, 0, 1);
        vm.prank(other);
        vm.expectRevert(ExitLease.NotOwner.selector);
        el.clearOffer(id);
    }

    function test_offerNeedsExitTag() public {
        vm.prank(seller);
        uint256 site = reg.register{value: 0.0002 ether}("myshop.sesh", "shop", "");
        vm.prank(seller);
        vm.expectRevert(ExitLease.NotExit.selector);
        el.setOffer(site, payout, WEI_H, 0, 24);
    }

    function test_offerBadParams() public {
        vm.startPrank(seller);
        vm.expectRevert(ExitLease.BadParams.selector);
        el.setOffer(id, address(0), WEI_H, 0, 24);
        vm.expectRevert(ExitLease.BadParams.selector);
        el.setOffer(id, payout, 0, 0, 24);
        vm.expectRevert(ExitLease.BadParams.selector);
        el.setOffer(id, payout, WEI_H, 0, 0);
        vm.expectRevert(ExitLease.BadParams.selector);
        el.setOffer(id, payout, WEI_H, 0, 24 * 31 + 1);
        vm.stopPrank();
        ExitLease noUsdc = new ExitLease(reg, address(0));
        vm.prank(seller);
        vm.expectRevert(ExitLease.BadParams.selector);
        noUsdc.setOffer(id, payout, 0, USDC_H, 24);
    }

    function test_clearedOffer_notForSale() public {
        vm.prank(seller);
        el.clearOffer(id);
        vm.expectRevert(ExitLease.NoOffer.selector);
        el.quote(id, address(0), 1);
    }

    function test_exitingListing_notForSale() public {
        vm.prank(seller);
        reg.requestExit(id);
        vm.prank(buyer);
        vm.expectRevert(ExitLease.NotExit.selector);
        el.buy{value: WEI_H}(id, address(0), 1, WEI_H, BSESH);
        // cancelling the exit puts it back on sale with the same offer
        vm.prank(seller);
        reg.cancelExit(id);
        vm.prank(buyer);
        el.buy{value: WEI_H}(id, address(0), 1, WEI_H, BSESH);
    }

    function test_removedByChallenge_notForSale() public {
        vm.prank(other);
        reg.challenge{value: 0.0001 ether}(id);
        vm.warp(block.timestamp + 2 days + 1);
        reg.resolveChallenge(id);
        vm.expectRevert(ExitLease.NotExit.selector);
        el.quote(id, address(0), 1);
    }

    function test_payoutRejectsEth_reverts() public {
        RejectEthPayout r = new RejectEthPayout();
        vm.prank(seller);
        el.setOffer(id, address(r), WEI_H, 0, 24);
        vm.prank(buyer);
        vm.expectRevert(ExitLease.TransferFailed.selector);
        el.buy{value: WEI_H}(id, address(0), 1, WEI_H, BSESH);
        assertEq(el.leaseCount(), 0);
    }

    function test_reentrancyBlocked() public {
        ReenterPayout r = new ReenterPayout();
        r.arm(el, id);
        vm.prank(seller);
        el.setOffer(id, address(r), WEI_H, 0, 24);
        vm.prank(buyer);
        el.buy{value: WEI_H}(id, address(0), 1, WEI_H, BSESH);
        assertTrue(r.tried());
        assertTrue(r.reverted());
        assertEq(el.leaseCount(), 1);
    }

    function test_separateBuyersSeparateClocks() public {
        string memory s2 = "5tg4wjp6qg6pyw9kstkkcwihpqhjh1ydzixjo78pjdditdcpyxfo.sesh";
        vm.prank(buyer);
        el.buy{value: WEI_H}(id, address(0), 1, WEI_H, BSESH);
        vm.prank(other);
        el.buy{value: WEI_H}(id, address(0), 1, WEI_H, s2);
        assertEq(el.lease(1).start, 1_000_000);
    }

    function testFuzz_endIsStartPlusHours(uint32 h, uint32 gap) public {
        h = uint32(bound(h, 1, 24));
        gap = uint32(bound(gap, 0, 3 days));
        vm.prank(buyer);
        el.buy{value: WEI_H}(id, address(0), 1, WEI_H, BSESH);
        vm.warp(1_000_000 + uint256(gap));
        uint256 amt = uint256(WEI_H) * h;
        vm.prank(buyer);
        el.buy{value: amt}(id, address(0), h, amt, BSESH);
        ExitLease.Lease memory l = el.lease(1);
        uint64 expStart = gap < 1 hours ? uint64(1_000_000 + 1 hours) : uint64(1_000_000 + uint256(gap));
        assertEq(l.start, expStart);
        assertEq(l.end, expStart + uint64(h) * 1 hours);
        assertEq(payout.balance, WEI_H + amt);
    }
}
